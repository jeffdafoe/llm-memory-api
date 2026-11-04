// Package server assembles the HTTP handler: the shared services, the
// middleware chain and the routes.
package server

import (
	"net/http"
	"os"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/events"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
)

// Services are the long-lived pieces routes share.
type Services struct {
	DB          db.DB
	Config      *config.Store
	Actors      *actors.Cache
	Log         *logger.Logger
	Events      events.Broadcaster
	Router      *httpapi.Router
	Sessions    *auth.Sessions
	APIKeys     *auth.APIKeys
	SessionAuth *auth.SessionAuth
	MCPAuth     *auth.MCPAuth
	Namespaces  *permissions.Namespaces
	Admin       *permissions.Admin
}

// NewServices wires the shared services over one database and config store.
// logDir is where activity.log goes; empty turns file logging off.
func NewServices(conn db.DB, store *config.Store, logDir string) *Services {
	actorCache := actors.NewCache(conn)
	log := logger.New(conn, actorCache, logDir)
	bc := events.Broadcaster(events.Nop{})
	sessions := &auth.Sessions{DB: conn}
	keys := &auth.APIKeys{DB: conn, Warnf: func(format string, args ...any) { log.Errorf(format, args...) }}
	return &Services{
		DB:       conn,
		Config:   store,
		Actors:   actorCache,
		Log:      log,
		Events:   bc,
		Router:   &httpapi.Router{Log: log},
		Sessions: sessions,
		APIKeys:  keys,
		SessionAuth: &auth.SessionAuth{
			Sessions: sessions,
			APIKeys:  keys,
			Errorf:   log.Errorf,
		},
		MCPAuth:    auth.NewMCPAuth(conn, store, actorCache, keys, bc),
		Namespaces: &permissions.Namespaces{DB: conn},
		Admin:      &permissions.Admin{DB: conn, Errorf: log.Errorf},
	}
}

// LogDir is LOG_DIR, or the production default.
func LogDir() string {
	dir := os.Getenv("LOG_DIR")
	if dir == "" {
		return logger.DefaultLogDir
	}
	return dir
}

// Handler builds the full middleware chain, outermost first:
//
//	Recover → WithState → ParseJSONBody → RequestLog → Recover → routes
//
// The order follows server.js: the JSON parser runs before the request log, so
// a rejected body is answered and never logged, and the log sees the parsed
// body for MCP method names. The inner Recover turns a panic in auth or a
// route into the 500 before the request log records the status, so the
// request is still logged, as Express logged a request its error handler
// answered. The outer one covers the layers above the log.
func Handler(s *Services) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		httpapi.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// /v1: every route behind the session middleware and the last_seen
	// heartbeat. Routes are added to v1 as they are ported.
	v1 := http.NewServeMux()
	v1.HandleFunc("/", httpapi.NotFound)
	mux.Handle("/v1/", s.SessionAuth.Middleware("/v1", auth.Heartbeat(s.DB, v1)))

	mux.HandleFunc("/", httpapi.NotFound)

	requestLog := &httpapi.RequestLog{DB: s.DB}
	return httpapi.Recover(httpapi.WithState(httpapi.ParseJSONBody(requestLog.Middleware(httpapi.Recover(mux)))))
}
