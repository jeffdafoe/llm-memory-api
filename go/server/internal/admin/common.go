// Package admin is Node's routes/admin.js: the /v1/admin routes of the admin
// dashboard. The accounts-and-configuration routes (Routes) and the content,
// log and live-event routes (a separate type in the same package) register
// on one admin mux and share the helpers in this file.
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/visibility"
)

// loginPath is the one admin path requireUser lets through without a web
// session, compared on the raw path below /v1 as Express's req.path is.
const loginPath = "/admin/login"

// NewMux returns the admin router: a mux for raw paths below /v1 whose
// fallback is Express's 404 page. Node mounts the admin router last on /v1,
// so an unmatched /v1 path, or a /v1 path whose router has no route for the
// method, reaches it; server.go makes Handler(mux) the /v1 mux's fallback.
func NewMux() *httpapi.RawMux {
	return httpapi.NewRawMux("/v1", http.HandlerFunc(httpapi.NotFound))
}

// Handler is the admin router as Node mounts it: router.use(requireUser) in
// front of every admin route, so a caller without an admin web session gets
// 401 before any permission check, the automatic OPTIONS answer or the 404,
// except on /admin/login.
func Handler(mux *httpapi.RawMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(httpapi.RawPath(r), "/v1") != loginPath && httpapi.StateOf(r).AuthenticatedUser == nil {
			httpapi.WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "Admin authentication required")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// Common is what both halves of the admin routes share.
type Common struct {
	DB         db.DB
	Log        *logger.Logger
	Router     *httpapi.Router
	Admin      *permissions.Admin
	Actors     *actors.Cache
	Visibility *visibility.Visibility
}

// Handle registers POST path (below /v1, "/admin/...") as
// requirePerm(resource, action) then adminRoute(label), both run by detach.
// An empty resource is a route Node registered with adminRoute alone.
func (c *Common) Handle(mux *httpapi.RawMux, path, resource, action, label string, fn httpapi.HandlerFunc) {
	mux.Handle(http.MethodPost, path, detach(c.route(resource, action, label, fn)))
}

// HandleUnbounded is Handle for a route that runs for minutes (one model
// call per item): the client hang-up still does not cancel it, and there
// is no routeTimeout, as Node had no limit.
func (c *Common) HandleUnbounded(mux *httpapi.RawMux, path, resource, action, label string, fn httpapi.HandlerFunc) {
	h := c.route(resource, action, label, fn)
	mux.Handle(http.MethodPost, path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(context.WithoutCancel(r.Context())))
	}))
}

func (c *Common) route(resource, action, label string, fn httpapi.HandlerFunc) http.Handler {
	var h http.Handler = c.Router.Route("admin", label, fn)
	if resource != "" {
		h = c.Admin.RequirePerm(resource, action, h)
	}
	return h
}

// routeTimeout bounds an admin route once it no longer ends with the request.
const routeTimeout = 30 * time.Second

// detach runs h on a context that a client hang-up does not cancel, bounded
// by routeTimeout. Node did not cancel a route's queries when the client
// hung up, so the permission, visibility and ownership checks and the write
// after them ran to the end; on the request context a hang-up during a
// check would fail the check and skip the write.
func detach(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), routeTimeout)
		defer cancel()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// LogAdmin is logAdmin: an activity line under the admin subsystem.
func (c *Common) LogAdmin(action string, details logger.Fields) {
	c.Log.Log("admin", action, details)
}

// User is req.authenticatedUser. Handler lets no request without one reach
// a route other than /admin/login.
func User(r *http.Request) *httpapi.User {
	return httpapi.StateOf(r).AuthenticatedUser
}

// RequireVisibility is requireVisibility: the actor named name when the
// caller may see it. A missing or hidden actor is the same 404, written
// directly as Node's res.status(404).json() was (not logged), and ok is
// false; the handler then returns nil.
func (c *Common) RequireVisibility(w http.ResponseWriter, r *http.Request, name string) (actor actors.Actor, ok bool, err error) {
	actor, found, err := c.Actors.ResolveByName(r.Context(), name)
	if err != nil {
		return actors.Actor{}, false, err
	}
	if found {
		allowed, err := c.Visibility.CanSee(r.Context(), httpapi.StateOf(r).ActorID, actor.ID)
		if err != nil {
			return actors.Actor{}, false, err
		}
		if allowed {
			return actor, true, nil
		}
	}
	httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", "Agent not found")
	return actors.Actor{}, false, nil
}

// RequireOwnership is requireOwnership: a superadmin, the caller itself, or
// the actor's creator may modify it. Otherwise it writes the 403 directly
// and returns false. target is the value Node passed as targetActorId, bound
// to the created_by query as node-postgres bound it; self is Node's
// targetActorId === req.actorId, which the caller decides, because a body
// value of another JSON type ("5" for 5) is not strictly equal.
func (c *Common) RequireOwnership(w http.ResponseWriter, r *http.Request, target any, self bool) (bool, error) {
	callerID := httpapi.StateOf(r).ActorID
	super, err := c.Admin.IsSuperadmin(r.Context(), callerID)
	if err != nil {
		return false, err
	}
	if super || self {
		return true, nil
	}
	var createdBy *int
	err = c.DB.QueryRow(r.Context(), "SELECT created_by FROM actors WHERE id = $1", target).Scan(&createdBy)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if err == nil && createdBy != nil && *createdBy == callerID {
		return true, nil
	}
	httpapi.WriteError(w, http.StatusForbidden, "FORBIDDEN", "You can only modify agents you created")
	return false, nil
}

// RequireOwnershipID is RequireOwnership for an actor ID the route already
// holds as an integer.
func (c *Common) RequireOwnershipID(w http.ResponseWriter, r *http.Request, target int) (bool, error) {
	return c.RequireOwnership(w, r, target, target == httpapi.StateOf(r).ActorID)
}

const maxSafeInteger = 1<<53 - 1

// SafeInt is util.js safeInt: a JSON number, or a string that is not blank,
// whose Number() is a safe integer. ok is false for anything else (Node's
// null), absent and JSON null included.
func SafeInt(raw json.RawMessage) (n int64, ok bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0, false
	}
	var f float64
	switch c := raw[0]; {
	case c == '"':
		var s string
		if json.Unmarshal(raw, &s) != nil || jsutil.Trim(s) == "" {
			return 0, false
		}
		f = jsutil.StringToNumber(s)
	case c == '-' || c >= '0' && c <= '9':
		f = jsutil.ToNumber(raw)
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f != math.Trunc(f) || math.Abs(f) > maxSafeInteger {
		return 0, false
	}
	return int64(f), true
}
