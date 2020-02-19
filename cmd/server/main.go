package main

import (
	"context"
	"encoding/base64"
	"log"
	"net/http"
	"regexp"
	"time"

	"github.com/99designs/gqlgen/handler"
	gkc "github.com/anselm94/googlekeepclone"
	gkcserver "github.com/anselm94/googlekeepclone/server"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/jinzhu/gorm"
	_ "github.com/jinzhu/gorm/dialects/sqlite"
	"github.com/rs/cors"
	"github.com/volatiletech/authboss"
	abclientstate "github.com/volatiletech/authboss-clientstate"
	"github.com/volatiletech/authboss/defaults"
)

var (
	config *gkc.AppConfig
	db     *gorm.DB
)

func main() {
	config = gkc.DefaultAppConfig()

	db = setupDB()
	defer db.Close()

	ab := setupAuthboss()

	handlerAuth := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			userID, _ := ab.CurrentUserID(r)
			ctx = context.WithValue(ctx, gkcserver.CtxUserIDKey, userID)
			h.ServeHTTP(w, r.WithContext(ctx))
		})
	}

	handlerCors := cors.New(cors.Options{
		AllowedOrigins: []string{
			config.AppHost.String(),
		},
		AllowCredentials: true,
	}).Handler

	handlerGraphQL := handler.GraphQL(
		gkcserver.NewExecutableSchema(gkcserver.Config{
			Resolvers: &gkcserver.Resolver{
				DB: db,
			},
		}),
		handler.WebsocketUpgrader(websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return r.Host == config.AppHost.Host
			},
		}),
		handler.WebsocketKeepAliveDuration(10*time.Second), // Don't drop websocket after being idle for few seconds https://github.com/99designs/gqlgen/issues/640
	)

	router := mux.NewRouter()
	router.Use(handlerCors, ab.LoadClientStateMiddleware, handlerAuth)
	router.Path("/playground").Handler(handler.Playground("Playground", "/query"))
	router.PathPrefix("/query").Handler(handlerGraphQL)
	router.PathPrefix("/auth").Handler(http.StripPrefix("/auth", ab.Config.Core.Router))
	router.PathPrefix("/").Handler(http.FileServer(http.Dir(config.StaticDir)))
	log.Fatalf("Error running server -> %s", http.ListenAndServe(":"+config.AppHost.Port(), router))
}

func setupDB() *gorm.DB {
	db, err := gorm.Open("sqlite3", config.DBFile)
	if err != nil {
		log.Fatalf("Error while setting up DB -> %s", err)
	}
	db.Exec("PRAGMA foreign_keys = ON;")
	db.AutoMigrate(&gkcserver.Todo{}, &gkcserver.Note{}, &gkcserver.Label{}, &gkcserver.User{})
	return db
}

func setupAuthboss() *authboss.Authboss {
	ab := authboss.New()
	ab.Config.Paths.Mount = "/auth"
	ab.Config.Paths.RootURL = config.AppHost.String()

	cookieStoreKey, _ := base64.StdEncoding.DecodeString(config.CookieStoreKey)
	sessionStoreKey, _ := base64.StdEncoding.DecodeString(config.SessionStoreKey)
	cookieStore := abclientstate.NewCookieStorer(cookieStoreKey, nil)
	cookieStore.HTTPOnly = config.IsProd
	cookieStore.Secure = config.IsProd
	sessionStore := abclientstate.NewSessionStorer(config.SessionCookieName, sessionStoreKey, nil)
	sqliteStorer := gkcserver.NewSQLiteStorer(db)

	ab.Config.Storage.Server = sqliteStorer
	ab.Config.Storage.SessionState = sessionStore
	ab.Config.Storage.CookieState = cookieStore
	ab.Config.Core.ViewRenderer = defaults.JSONRenderer{}

	ab.Config.Modules.RegisterPreserveFields = []string{"email", "name"}
	ab.Config.Modules.ResponseOnUnauthed = authboss.RespondRedirect

	defaults.SetCore(&ab.Config, true, false)

	pidRule := defaults.Rules{
		FieldName: "username", Required: true,
		MatchError: "Usernames must only start with letters, and contain letters and numbers",
		MustMatch:  regexp.MustCompile(`(?i)[a-z][a-z0-9]?`),
	}
	emailRule := defaults.Rules{
		FieldName: "email", Required: false,
		MatchError: "Must be a valid e-mail address",
		MustMatch:  regexp.MustCompile(`.*@.*\.[a-z]+`),
	}
	passwordRule := defaults.Rules{
		FieldName: "password", Required: true,
		MinLength: 4,
	}
	nameRule := defaults.Rules{
		FieldName: "name", Required: false,
		MinLength: 2,
	}

	ab.Config.Core.BodyReader = defaults.HTTPBodyReader{
		ReadJSON:    false,
		UseUsername: true,
		Rulesets: map[string][]defaults.Rules{
			"login":    {pidRule},
			"register": {pidRule, emailRule, passwordRule, nameRule},
		},
		Whitelist: map[string][]string{
			"register": {"username", "email", "name", "password"},
		},
	}

	if err := ab.Init(); err != nil {
		log.Fatalf("Error while initialising Authboss -> %s", err)
	}
	return ab
}