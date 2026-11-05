package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
)

// Actor is the caller as the namespace permission checks name it (Node's
// getActor in the route files).
type Actor struct {
	ID   int
	Name string
	Kind string // "agent" or "user"
}

// ActorOf is the authenticated agent, else the admin user, else "unknown"
// with kind "user".
func ActorOf(r *http.Request) Actor {
	st := StateOf(r)
	a := Actor{ID: st.ActorID, Name: st.AuthenticatedAgent, Kind: "agent"}
	if a.Name == "" {
		a.Kind = "user"
		if st.AuthenticatedUser != nil && st.AuthenticatedUser.Username != "" {
			a.Name = st.AuthenticatedUser.Username
		} else {
			a.Name = "unknown"
		}
	}
	return a
}

// BadRequest answers a missing required field. Node wrote these 400s with
// res.status(400).json() instead of throwing, so the route wrapper never
// logged them to error_log. The handler returns nil after it.
func BadRequest(w http.ResponseWriter, message string) error {
	WriteError(w, http.StatusBadRequest, "BAD_REQUEST", message)
	return nil
}

// NoBody is the TypeError Node threw when a route destructured a field from
// an undefined req.body (no JSON body). The text is V8's, so the error_log
// row matches.
func NoBody(field string) error {
	return errors.New("Cannot destructure property '" + field + "' of 'req.body' as it is undefined.")
}

// NotString refuses a field of a JSON type the route cannot use. Node passed
// such a value on, where it was coerced to text or failed in a later step;
// the port refuses it (go-server note, Known differences).
func NotString(name string) error {
	return apierr.New(http.StatusBadRequest, fmt.Sprintf("%s must be a string", name))
}

// RequireString reads a field the route needs as a string, after the
// route's required-fields check has passed.
func (f Fields) RequireString(name string) (string, error) {
	s, ok := f.String(name)
	if !ok {
		return "", NotString(name)
	}
	return s, nil
}

// OptionalString reads a field that may be absent: a falsy value of any
// type is "" (absent), a string is returned, and a truthy value of another
// type is refused.
func (f Fields) OptionalString(name string) (string, error) {
	if !f.Truthy(name) {
		if s, ok := f.String(name); ok {
			return s, nil
		}
		return "", nil
	}
	return f.RequireString(name)
}
