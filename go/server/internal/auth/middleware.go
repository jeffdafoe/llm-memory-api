package auth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/ttlcache"
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

// lookupTimeout bounds a token lookup, which a client hang-up does not stop.
const lookupTimeout = 30 * time.Second

// lookupAttempts bounds the retries when invalidations keep landing during a
// lookup. Past it the token is refused: every result read may describe a
// session revoked meanwhile.
const lookupAttempts = 3

// SessionAuth guards /v1. A bearer token is tried, in order, as an agent API
// session, an admin web session, and an API key. A hit is cached for up to
// five minutes (never past the session's own expiry).
type SessionAuth struct {
	Sessions *Sessions
	APIKeys  *APIKeys
	// Errorf receives lookup errors, which are logged and then treated as a
	// miss so the next credential type is still tried.
	Errorf func(format string, args ...any)

	// cache.Now is also the clock for computing expiries; tests replace it.
	cache ttlcache.Cache[string, cachedSession]
}

func (a *SessionAuth) now() time.Time {
	if a.cache.Now != nil {
		return a.cache.Now()
	}
	return time.Now()
}

func (a *SessionAuth) errorf(format string, args ...any) {
	if a.Errorf != nil {
		a.Errorf(format, args...)
	}
}

// Forget drops a token from the cache. Logout and passphrase rotation call it
// so a revoked token stops working at once. A lookup of any token that was in
// flight when Forget ran is repeated rather than trusted.
func (a *SessionAuth) Forget(token string) {
	a.cache.Delete(token)
}

// ForgetAgent drops every cached token of one agent (API sessions and API
// keys). Passphrase rotation calls it after deleting the agent's sessions.
// Like Node's loop over the cache, it also drops the agent's API-key entries,
// which the next request then looks up again.
func (a *SessionAuth) ForgetAgent(agent string) {
	a.cache.DeleteFunc(func(_ string, s cachedSession) bool {
		return !s.isUser && s.agent == agent
	})
}

// ForgetActor drops every cached token of one actor: its API sessions, web
// sessions and API keys. Actor deletion calls it after deleting the rows.
func (a *SessionAuth) ForgetActor(actorID int) {
	a.cache.DeleteFunc(func(_ string, s cachedSession) bool {
		return s.actorID == actorID
	})
}

// CachedUser is the admin user a token is cached for, without a database
// lookup: the WebSocket upgrade reads the middleware's cache first, as
// events.js read authMiddleware.sessionCache. An agent entry is not a user.
func (a *SessionAuth) CachedUser(token string) (httpapi.User, bool) {
	entry, ok := a.cache.Get(token)
	if !ok || !entry.isUser {
		return httpapi.User{}, false
	}
	return entry.user, true
}

func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// resolve returns the session for a token, from the cache or the database.
// A database result is used and cached only if no Forget ran during the
// lookup; if one did, the lookup is repeated, because the result may describe
// a session that was just revoked.
func (a *SessionAuth) resolve(ctx context.Context, token string) (cachedSession, bool) {
	if entry, ok := a.cache.Get(token); ok {
		return entry, true
	}
	for attempt := 0; attempt < lookupAttempts; attempt++ {
		gen := a.cache.Generation()
		entry, found := a.lookup(ctx, token)
		if !found {
			return cachedSession{}, false
		}
		if a.cache.SetIfCurrent(gen, token, entry, entry.expires) {
			return entry, true
		}
	}
	a.errorf("Auth: refused a token after %d lookups were each invalidated by a concurrent logout", lookupAttempts)
	return cachedSession{}, false
}

// Middleware wraps the /v1 routes. prefix is the mount path ("/v1"); the
// unauthenticated paths are matched below it on the raw path, like Express's
// req.path, so "/v1/agent%2Flogin" still needs a token.
func (a *SessionAuth) Middleware(prefix string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unauthenticatedV1[strings.TrimPrefix(httpapi.RawPath(r), prefix)] {
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

		// The lookup runs on after the client hangs up, as Node's queries did:
		// on the request's context a cancelled query reads as a miss and
		// refuses a request whose token is valid. The route still gets the
		// request's context.
		lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), lookupTimeout)
		entry, found := a.resolve(lookupCtx, token)
		cancel()
		if !found {
			httpapi.WriteError(w, http.StatusForbidden, "FORBIDDEN", "Invalid or expired session token")
			return
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
