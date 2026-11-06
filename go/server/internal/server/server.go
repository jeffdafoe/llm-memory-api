// Package server assembles the HTTP handler: the shared services, the
// middleware chain and the routes.
package server

import (
	"context"
	"net/http"
	"os"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/agent"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/chat"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/discussion"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/events"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/mail"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/memory"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/oauth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/registration"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/syserrors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/system"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/virtualagent"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/visibility"
)

// Services are the long-lived pieces routes share.
type Services struct {
	DB          db.DB
	Config      *config.Store
	Actors      *actors.Cache
	Log         *logger.Logger
	Events      events.Broadcaster
	Hub         *events.Hub
	Router      *httpapi.Router
	Sessions    *auth.Sessions
	APIKeys     *auth.APIKeys
	SessionAuth *auth.SessionAuth
	MCPAuth     *auth.MCPAuth
	Namespaces  *permissions.Namespaces
	Admin       *permissions.Admin
	Errors      *syserrors.Recorder
	Memory      *memory.Service
	Notes       *notes.Service
	Mail        *mail.Service
	System      *system.Handler
	Chat        *chat.Service
	Discussions *discussion.Service
	Providers   *providers.Providers
	Agents      *virtualagent.Service
	Visibility  *visibility.Visibility
}

// NewServices wires the shared services over one database and config store.
// logDir is where activity.log goes; empty turns file logging off.
func NewServices(conn db.DB, store *config.Store, logDir string) *Services {
	actorCache := actors.NewCache(conn)
	log := logger.New(conn, actorCache, logDir)
	recorder := &syserrors.Recorder{DB: conn, Actors: actorCache, Errorf: log.Errorf}
	// The token check needs the session cache built below; it is set once
	// the services exist.
	hub := events.NewHub(nil, log.Errorf)
	bc := events.Broadcaster(hub)
	sessions := &auth.Sessions{DB: conn}
	keys := &auth.APIKeys{DB: conn, Warnf: func(format string, args ...any) { log.Errorf(format, args...) }}
	mem := &memory.Service{DB: conn, Config: store, Embedder: &memory.OpenAI{}, Errors: recorder}
	agents := virtualagent.Seam{}
	llm := &providers.Providers{Config: store, Log: log}
	notesService := &notes.Service{
		DB: conn, Config: store, Actors: actorCache, Memory: mem, Errors: recorder, Events: bc,
		Enricher: &notes.GatedEnricher{Config: store},
	}
	// Mail and the virtual agents call each other: a mail to a virtual
	// agent triggers its reply, which is sent as mail.
	mailService := &mail.Service{DB: conn, Actors: actorCache, Log: log}
	vaService := &virtualagent.Service{
		DB: conn, Config: store, Actors: actorCache, Log: log, Events: bc, Providers: llm,
		Notes: notesService, Memory: mem, Mail: mailService,
	}
	mailService.Agents = vaService
	sysHandler := &system.Handler{Log: log}
	sysHandler.Register("virtual-agent", agents.HandleVirtualAgent)
	admin := &permissions.Admin{DB: conn, Errorf: log.Errorf}
	notifier := &system.Notifier{DB: conn, Actors: actorCache, Handler: sysHandler}
	// Chat calls the virtual agents the same way: a chat message to one is
	// answered through HandleDirectChat, which sends its reply as chat.
	chatService := &chat.Service{
		DB: conn, Actors: actorCache, Log: log, Events: bc, Agents: vaService, System: sysHandler,
		Notifier: notifier,
	}
	vaService.Chat = chatService
	s := &Services{
		DB:       conn,
		Config:   store,
		Actors:   actorCache,
		Log:      log,
		Events:   bc,
		Hub:      hub,
		Router:   &httpapi.Router{Log: log},
		Sessions: sessions,
		APIKeys:  keys,
		SessionAuth: &auth.SessionAuth{
			Sessions: sessions,
			APIKeys:  keys,
			Errorf:   log.Errorf,
		},
		MCPAuth:     auth.NewMCPAuth(conn, store, actorCache, keys, bc),
		Namespaces:  &permissions.Namespaces{DB: conn},
		Admin:       admin,
		Errors:      recorder,
		Memory:      mem,
		Notes:       notesService,
		Mail:        mailService,
		System:      sysHandler,
		Chat:        chatService,
		Discussions: &discussion.Service{DB: conn, Config: store, Actors: actorCache, Log: log, Notifier: notifier},
		Providers:   llm,
		Agents:      vaService,
		Visibility:  &visibility.Visibility{DB: conn, Admin: admin},
	}
	hub.Validate = adminTokenValidator(s.SessionAuth, sessions, log.Errorf)
	return s
}

// adminTokenValidator is events.js's validateAdminToken: the middleware's
// cached admin user for the token, else a web session looked up (and slid)
// as validateSessionToken does. A lookup error is written to stderr and
// refuses the token.
func adminTokenValidator(cache *auth.SessionAuth, sessions *auth.Sessions, errorf func(string, ...any)) events.TokenValidator {
	return func(ctx context.Context, token string) (string, bool) {
		if user, ok := cache.CachedUser(token); ok {
			return user.Username, true
		}
		row, err := sessions.Validate(ctx, token, auth.SessionKindWeb)
		if err != nil {
			errorf("WebSocket auth error: %s", err)
			return "", false
		}
		if row == nil {
			return "", false
		}
		return row.Name, true
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
	// /v1: every route behind the session middleware and the last_seen
	// heartbeat, matched on the raw path below /v1. A path no route has goes
	// to the /v1 fallback.
	v1 := httpapi.NewRawMux("/v1", http.HandlerFunc(v1Unmatched))
	(&memory.Routes{Service: s.Memory, Router: s.Router, Namespaces: s.Namespaces}).Register(v1)
	(&notes.Routes{Service: s.Notes, Router: s.Router, Namespaces: s.Namespaces}).Register(v1)
	(&mail.Routes{Service: s.Mail, Router: s.Router}).Register(v1)
	(&chat.Routes{Service: s.Chat, Router: s.Router}).Register(v1)
	(&discussion.Routes{Service: s.Discussions, Router: s.Router}).Register(v1)
	(&agent.Routes{
		DB: s.DB, Config: s.Config, Log: s.Log, Router: s.Router, Events: s.Events,
		Sessions: s.Sessions, SessionAuth: s.SessionAuth, Notes: s.Notes,
		Namespaces: s.Namespaces, Visibility: s.Visibility, Agents: s.Agents,
	}).Register(v1)
	v1Chain := s.SessionAuth.Middleware("/v1", auth.Heartbeat(s.DB, v1))

	// Top-level routes outside /v1, with no auth: OAuth (mounted before /mcp
	// and /v1 in Node) and registration (after them; no path overlaps).
	top := httpapi.NewRawMux("", http.HandlerFunc(httpapi.NotFound))
	oauth.New(s.DB, s.Config, s.Actors, s.APIKeys, s.Log, os.Getenv("BASE_URL")).Register(top)
	(&registration.Routes{
		DB: s.DB, Config: s.Config, Actors: s.Actors, Log: s.Log, Mail: s.Mail, Notes: s.Notes,
		Moderator: virtualagent.Moderator{DB: s.DB, Log: s.Log},
	}).Register(top)

	// The top level is decided on the raw path, as Express matches app.get
	// and app.use mounts: "/%76%31/x" is not under /v1 and "/%68ealth" is
	// not /health, though both decode to those paths. "/v1" itself is under
	// the mount.
	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := httpapi.RawPath(r)
		switch {
		case p == "/health" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
			httpapi.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		case p == "/health" && r.Method == http.MethodOptions:
			// Express's router answers OPTIONS itself for a path with routes,
			// listing their methods.
			httpapi.WriteOptions(w, "GET, HEAD")
		case p == "/v1" || strings.HasPrefix(p, "/v1/"):
			v1Chain.ServeHTTP(w, r)
		case top.Handles(p):
			top.ServeHTTP(w, r)
		default:
			httpapi.NotFound(w, r)
		}
	})

	requestLog := &httpapi.RequestLog{DB: s.DB}
	chain := httpapi.Recover(httpapi.WithState(httpapi.ParseJSONBody(requestLog.Middleware(httpapi.Recover(root)))))
	// server.js has an 'upgrade' listener, so Node hands every upgrade
	// request to it before Express: none is parsed, logged or routed.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if events.IsUpgrade(r) {
			s.Hub.ServeUpgrade(w, r)
			return
		}
		chain.ServeHTTP(w, r)
	})
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
