package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
)

// sessionCacheTTL bounds how long a validated token skips the database.
const sessionCacheTTL = 5 * time.Minute

// unauthenticatedV1 are the /v1 paths that need no token.
var unauthenticatedV1 = map[string]bool{
	"/agent/login": true,
	"/admin/login": true,
}

type cachedSession struct {
	isUser  bool
	agent   string
	user    httpapi.User
	actorID int
	expires time.Time
	method  string
}

// SessionAuth guards /v1. A bearer token is tried, in order, as an agent API
// session, an admin web session, and an API key. A hit is cached for up to
// five minutes (never past the session's own expiry).
type SessionAuth struct {
	Sessions *Sessions
	APIKeys  *APIKeys
	// Errorf receives lookup errors, which are logged and then treated as a
	// miss so the next credential type is still tried.
	Errorf func(format string, args ...any)
	Now    func() time.Time

	mu    sync.Mutex
	cache map[string]cachedSession
}

func (a *SessionAuth) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *SessionAuth) errorf(format string, args ...any) {
	if a.Errorf != nil {
		a.Errorf(format, args...)
	}
}

// Forget drops a token from the cache. Logout and passphrase rotation call it
// so a revoked token stops working at once.
func (a *SessionAuth) Forget(token string) {
	a.mu.Lock()
	delete(a.cache, token)
	a.mu.Unlock()
}

func (a *SessionAuth) cached(token string) (cachedSession, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.cache[token]
	if ok && entry.expires.After(a.now()) {
		return entry, true
	}
	return cachedSession{}, false
}

func (a *SessionAuth) remember(token string, entry cachedSession) {
	a.mu.Lock()
	if a.cache == nil {
		a.cache = map[string]cachedSession{}
	}
	a.cache[token] = entry
	a.mu.Unlock()
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Middleware wraps the /v1 routes. prefix is the mount path ("/v1"); the
// unauthenticated paths are matched below it, like Express's req.path.
func (a *SessionAuth) Middleware(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unauthenticatedV1[strings.TrimPrefix(r.URL.Path, prefix)] {
			next.ServeHTTP(w, r)
			return
		}

		header := r.Header.Get("Authorization")
		if header == "" {
			httpapi.WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "Missing Authorization header")
			return
		}
		// The first "Bearer " anywhere in the header is removed, not only a
		// leading one, exactly as the Node code's header.replace did.
		token := strings.Replace(header, "Bearer ", "", 1)

		entry, ok := a.cached(token)
		if !ok {
			var found bool
			entry, found = a.lookup(r.Context(), token)
			if !found {
				httpapi.WriteError(w, http.StatusForbidden, "FORBIDDEN", "Invalid or expired session token")
				return
			}
			a.remember(token, entry)
		}

		st := httpapi.StateOf(r)
		st.AuthMethod = entry.method
		st.ActorID = entry.actorID
		if entry.isUser {
			user := entry.user
			st.AuthenticatedUser = &user
		} else {
			st.AuthenticatedAgent = entry.agent
		}
		next.ServeHTTP(w, r)
	})
}

func (a *SessionAuth) lookup(ctx context.Context, token string) (cachedSession, bool) {
	now := a.now()

	apiRow, err := a.Sessions.Validate(ctx, token, SessionKindAPI)
	if err != nil {
		a.errorf("Auth API session lookup error: %s", err)
	} else if apiRow != nil {
		return cachedSession{
			agent:   apiRow.Name,
			actorID: apiRow.ActorID,
			expires: earlier(now.Add(sessionCacheTTL), apiRow.ExpiresAt),
			method:  "session",
		}, true
	}

	webRow, err := a.Sessions.Validate(ctx, token, SessionKindWeb)
	if err != nil {
		a.errorf("Auth web session lookup error: %s", err)
	} else if webRow != nil {
		return cachedSession{
			isUser:  true,
			user:    httpapi.User{ID: webRow.ActorID, Username: webRow.Name},
			actorID: webRow.ActorID,
			expires: earlier(now.Add(sessionCacheTTL), webRow.ExpiresAt),
			method:  "session",
		}, true
	}

	// API keys let services (the salem engine) authenticate as an agent
	// without refreshing a 24-hour session.
	key, err := a.APIKeys.Find(ctx, token, 0)
	if err != nil {
		a.errorf("Auth API key lookup error: %s", err)
	} else if key != nil {
		return cachedSession{
			agent:   key.Agent,
			actorID: key.ActorID,
			expires: now.Add(sessionCacheTTL),
			method:  "api-key",
		}, true
	}
	return cachedSession{}, false
}

// Heartbeat stamps actors.last_seen for any authenticated /v1 request, agent
// or admin user, in the background.
//
// In Node the session middleware also stamped agents itself, so an agent
// request wrote last_seen twice; one write gives the same row.
func Heartbeat(conn db.DB, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := httpapi.StateOf(r)
		actorID := st.ActorID
		if actorID == 0 {
			actorID = st.MCPActorID
		}
		if actorID != 0 {
			db.Background(func(ctx context.Context) {
				conn.Exec(ctx, "UPDATE actors SET last_seen = NOW() WHERE id = $1", actorID)
			})
		}
		next.ServeHTTP(w, r)
	})
}
