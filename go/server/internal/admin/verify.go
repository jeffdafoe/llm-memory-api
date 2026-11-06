package admin

import (
	"net/http"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
)

// VerifyPath is POST /v1/auth/verify (Node's routes/auth.js), which Node
// mounts on /v1 before the session middleware: the token in the body is the
// credential, so the request needs no Authorization header.
const VerifyPath = "/v1/auth/verify"

// Verify lets external services (the salem engine's umbilical) check a
// session token and learn whose it is, with that actor's admin-permission
// map, without a copy of the session logic.
type Verify struct {
	Sessions *auth.Sessions
	Admin    *permissions.Admin
	Log      *logger.Logger
}

type verifyResponse struct {
	Valid       bool                      `json:"valid"`
	Agent       string                    `json:"agent"`
	ActorID     int                       `json:"actor_id"`
	Realms      []string                  `json:"realms"`
	SessionKind string                    `json:"session_kind"`
	Permissions permissions.PermissionMap `json:"permissions"`
}

type verifyRefusal struct {
	Valid bool   `json:"valid"`
	Error string `json:"error,omitempty"`
}

// Mux is the router Node mounted for routes/auth.js: POST /auth/verify,
// with Express's automatic OPTIONS answer. Any other method falls through
// to fallback, as the request went on to the session middleware.
func (v *Verify) Mux(fallback http.Handler) *httpapi.RawMux {
	mux := httpapi.NewRawMux("/v1", fallback)
	mux.Handle(http.MethodPost, "/auth/verify", http.HandlerFunc(v.verify))
	return mux
}

func (v *Verify) verify(w http.ResponseWriter, r *http.Request) {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		v.Log.Errorf("Unhandled error: %s", httpapi.NoBody("token"))
		httpapi.WriteUnhandledError(w, http.StatusInternalServerError, httpapi.NoBody("token").Error())
		return
	}
	if !f.Truthy("token") {
		httpapi.WriteJSON(w, http.StatusBadRequest, verifyRefusal{Error: "Missing token"})
		return
	}
	token, isString := f.String("token")
	if !isString {
		httpapi.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "token must be a string")
		return
	}
	// Web sessions (admin users in a browser) first, then API sessions
	// (agents that log in programmatically).
	for _, kind := range []string{auth.SessionKindWeb, auth.SessionKindAPI} {
		row, err := v.Sessions.Validate(r.Context(), token, kind)
		if err != nil {
			v.fail(w, err)
			return
		}
		if row == nil {
			continue
		}
		perms, err := v.Admin.PermissionMap(r.Context(), row.ActorID)
		if err != nil {
			v.fail(w, err)
			return
		}
		realms := row.Realms
		if realms == nil {
			realms = []string{}
		}
		httpapi.WriteJSON(w, http.StatusOK, verifyResponse{
			Valid: true, Agent: row.Name, ActorID: row.ActorID, Realms: realms,
			SessionKind: kind, Permissions: perms,
		})
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, verifyRefusal{})
}

func (v *Verify) fail(w http.ResponseWriter, err error) {
	v.Log.Errorf("Auth verify error: %s", httpapi.ErrorMessage(err))
	httpapi.WriteJSON(w, http.StatusInternalServerError, verifyRefusal{Error: "Internal error"})
}
