package auth

import (
	"testing"
	"time"
)

// ForgetAgent drops the agent's API-session and API-key entries, and
// nothing of other agents or of admin web sessions.
func TestForgetAgent(t *testing.T) {
	a := &SessionAuth{}
	far := time.Now().Add(time.Hour)
	put := func(token string, s cachedSession) {
		a.cache.SetIfCurrent(a.cache.Generation(), token, s, far)
	}
	put("session-a", cachedSession{agent: "agent-a", method: "session"})
	put("key-a", cachedSession{agent: "agent-a", method: "api-key"})
	put("session-b", cachedSession{agent: "agent-b", method: "session"})
	put("web", cachedSession{isUser: true})
	a.ForgetAgent("agent-a")
	for token, want := range map[string]bool{"session-a": false, "key-a": false, "session-b": true, "web": true} {
		if _, ok := a.cache.Get(token); ok != want {
			t.Errorf("%s: cached %v", token, ok)
		}
	}
}
