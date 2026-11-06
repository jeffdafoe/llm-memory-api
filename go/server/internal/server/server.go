// Package server assembles the HTTP handler: the shared services, the
// middleware chain and the routes.
package server

import (
	"net/http"
	"os"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/admin"
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
	bc := events.Broadcaster(events.Nop{})
	sessions := &auth.Sessions{DB: conn}
	keys := &auth.APIKeys{DB: conn, Warnf: func(format string, args ...any) { log.Errorf(format, args...) }}
	mem := &memory.Service{DB: conn, Config: store, Embedder: &memory.OpenAI{}, Errors: recorder}
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
	// A discussion's virtual participants answer through the system
	// handler's "virtual-agent" messages.
	sysHandler.Register("virtual-agent", vaService.HandleVirtualAgent)
	admin := &permissions.Admin{DB: conn, Errorf: log.Errorf}
	notifier := &system.Notifier{DB: conn, Actors: actorCache, Handler: sysHandler}
	// Chat calls the virtual agents the same way: a chat message to one is
	// answered through HandleDirectChat, which sends its reply as chat.
	chatService := &chat.Service{
		DB: conn, Actors: actorCache, Log: log, Events: bc, Agents: vaService, System: sysHandler,
		Notifier: notifier,
	}
	vaService.Chat = chatService
	discussions := &discussion.Service{DB: conn, Config: store, Actors: actorCache, Log: log, Notifier: notifier}
	vaService.Discussions = discussions
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
		MCPAuth:     auth.NewMCPAuth(conn, store, actorCache, keys, bc),
		Namespaces:  &permissions.Namespaces{DB: conn},
		Admin:       admin,
		Errors:      recorder,
		Memory:      mem,
		Notes:       notesService,
		Mail:        mailService,
		System:      sysHandler,
		Chat:        chatService,
		Discussions: discussions,
		Providers:   llm,
		Agents:      vaService,
		Visibility:  &visibility.Visibility{DB: conn, Admin: admin},
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
	// The admin router, mounted last on /v1 in Node: a /v1 path no earlier
	// router answered reaches it, behind requireUser.
	adminMux := admin.NewMux()
	adminCommon := &admin.Common{
		DB: s.DB, Log: s.Log, Router: s.Router, Admin: s.Admin, Actors: s.Actors, Visibility: s.Visibility,
	}
	(&admin.Routes{
		Common: adminCommon, Config: s.Config, Sessions: s.Sessions, SessionAuth: s.SessionAuth,
		Providers: s.Providers, Namespaces: s.Namespaces,
	}).Register(adminMux)

	// /v1: every route behind the session middleware and the last_seen
	// heartbeat, matched on the raw path below /v1. A path no route has goes
	// to the admin router.
	v1 := httpapi.NewRawMux("/v1", admin.Handler(adminMux))
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
	// routes/auth.js is mounted on /v1 before the session middleware, so
	// /v1/auth/verify needs no Authorization header; its other methods go
	// on to the middleware.
	authMux := (&admin.Verify{Sessions: s.Sessions, Admin: s.Admin, Log: s.Log}).Mux(v1Chain)

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
		case p == admin.VerifyPath:
			authMux.ServeHTTP(w, r)
		case p == "/v1" || strings.HasPrefix(p, "/v1/"):
			v1Chain.ServeHTTP(w, r)
		case top.Handles(p):
			top.ServeHTTP(w, r)
		default:
			httpapi.NotFound(w, r)
		}
	})

	requestLog := &httpapi.RequestLog{DB: s.DB}
	return httpapi.Recover(httpapi.WithState(httpapi.ParseJSONBody(requestLog.Middleware(httpapi.Recover(root)))))
}
