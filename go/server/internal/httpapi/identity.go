package httpapi

import (
	"net/http"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sanitize"
)

// Identity is the agent a mail or chat call acts as. With an agent session
// (or API key) it is the authenticated agent: a truthy field that does not
// sanitize to that name is 403 IDENTITY_MISMATCH, written directly as Node
// did (a value of another JSON type is never equal to it). An admin web
// session may name any agent. done means the response is written.
func Identity(w http.ResponseWriter, r *http.Request, f Fields, field, mismatch string) (name string, done bool, err error) {
	agent := StateOf(r).AuthenticatedAgent
	if agent == "" {
		name, err := AgentField(f, field)
		return name, false, err
	}
	if f.Truthy(field) {
		s, isString := f.String(field)
		if given := sanitize.AgentName(s); !isString || given != "" && given != agent {
			WriteError(w, http.StatusForbidden, "IDENTITY_MISMATCH", mismatch)
			return "", true, nil
		}
	}
	return agent, false, nil
}

// AgentField is an optional agent-name field, sanitized.
func AgentField(f Fields, field string) (string, error) {
	s, err := f.OptionalString(field)
	return sanitize.AgentName(s), err
}
