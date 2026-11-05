// Package server assembles the HTTP handler: the shared services, the
// middleware chain and the routes.
package server

import (
	"net/http"
	"os"
	"strings"

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
	// heartbeat. Routes are added to v1 as they are ported. Express mounts
	// /v1 for "/v1" itself too, so the bare path gets the same chain instead
	// of ServeMux's redirect to "/v1/".
	v1 := http.NewServeMux()
	v1Fallback := http.HandlerFunc(v1Unmatched)
	v1.Handle("/", v1Fallback)
	v1Chain := s.SessionAuth.Middleware("/v1", auth.Heartbeat(s.DB, httpapi.Unclean(v1, v1Fallback)))
	mux.Handle("/v1/", v1Chain)
	mux.Handle("/v1", v1Chain)

	mux.HandleFunc("/", httpapi.NotFound)

	// A path ServeMux would clean and redirect never reaches it. Under /v1
	// (matched on the raw path, as Express's mount is) it still goes through
	// auth to the /v1 fallback; anywhere else it is the 404 page.
	root := httpapi.Unclean(mux, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := httpapi.RawPath(r); p == "/v1" || strings.HasPrefix(p, "/v1/") {
			v1Chain.ServeHTTP(w, r)
			return
		}
		httpapi.NotFound(w, r)
	}))

	requestLog := &httpapi.RequestLog{DB: s.DB}
	return httpapi.Recover(httpapi.WithState(httpapi.ParseJSONBody(requestLog.Middleware(httpapi.Recover(root)))))
}

// v1Unmatched answers a /v1 path no route matched. In Node the admin router
// is mounted last on /v1 with router.use(requireUser) (routes/admin.js), so
// every unmatched /v1 path reaches it: a caller without an admin web session
// gets 401 there, except on /admin/login, and an admin user falls through to
// the 404 page.
func v1Unmatched(w http.ResponseWriter, r *http.Request) {
	if strings.TrimPrefix(httpapi.RawPath(r), "/v1") != "/admin/login" && httpapi.StateOf(r).AuthenticatedUser == nil {
		httpapi.WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "Admin authentication required")
		return
	}
	httpapi.NotFound(w, r)
}
