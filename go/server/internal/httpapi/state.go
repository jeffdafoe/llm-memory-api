package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
)

// User is an admin web-session principal.
type User struct {
	ID       int    `json:"id"`
	Username string `json:"username"`
}

// State is what the middleware learns about a request: its parsed body and
// who sent it. One State is created per request by WithState, before any
// other middleware, and every later layer writes into the same pointer. That
// lets the request log (which wraps the routes) see the identity the auth
// middleware (which runs inside it) found, the way Express code shares fields
// on req.
type State struct {
	// Body is the raw JSON body when the request was application/json, nil
	// otherwise. An empty JSON body is stored as "{}".
	Body json.RawMessage

	// Set by the /v1 session middleware.
	AuthMethod         string // "session" or "api-key"
	ActorID            int    // 0 when not authenticated
	AuthenticatedAgent string
	AuthenticatedUser  *User

	// Set by the /mcp middleware.
	MCPAgent       string
	MCPActorID     int
	MCPPermissions []string
}

type stateKey struct{}

// WithState attaches a fresh State to every request.
func WithState(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), stateKey{}, &State{})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// StateOf returns the request's State. Handlers outside WithState (unit tests
// that call a handler directly) get a fresh empty State.
func StateOf(r *http.Request) *State {
	st, ok := r.Context().Value(stateKey{}).(*State)
	if !ok {
		return &State{}
	}
	return st
}

// WithStateValue returns a copy of r carrying st, for tests that need to set
// up an authenticated request without running the middleware.
func WithStateValue(r *http.Request, st *State) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), stateKey{}, st))
}
