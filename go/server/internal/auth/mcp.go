package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"os"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/events"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/signup"
)

// MCPAuth guards /mcp. It accepts an HMAC bearer token ("agent:hmac_hex",
// issued by /oauth/token, never expires) or an API key, from the
// Authorization header or a ?token= query parameter.
type MCPAuth struct {
	DB      db.DB
	Config  *config.Store
	Actors  *actors.Cache
	APIKeys *APIKeys
	Events  events.Broadcaster
	// BaseURL is the public origin for the OAuth resource-metadata hint. When
	// empty it is built from the request, as http://<Host> (nginx terminates
	// TLS, so the server itself only ever sees plain HTTP).
	BaseURL string
}

// NewMCPAuth reads BASE_URL from the environment, as the Node server did.
func NewMCPAuth(conn db.DB, store *config.Store, actorCache *actors.Cache, keys *APIKeys, bc events.Broadcaster) *MCPAuth {
	return &MCPAuth{DB: conn, Config: store, Actors: actorCache, APIKeys: keys, Events: bc, BaseURL: os.Getenv("BASE_URL")}
}

type oauthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (m *MCPAuth) resourceMetadataURL(r *http.Request) string {
	if m.BaseURL != "" {
		return m.BaseURL + "/.well-known/oauth-protected-resource"
	}
	return "http://" + r.Host + "/.well-known/oauth-protected-resource"
}

func (m *MCPAuth) challenge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+m.resourceMetadataURL(r)+`"`)
}

// HMACToken returns the bearer token for an agent under secret.
func HMACToken(secret, agent string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(agent))
	return agent + ":" + hex.EncodeToString(mac.Sum(nil))
}

// verifyHMAC returns the agent name when token is a valid HMAC bearer token.
func (m *MCPAuth) verifyHMAC(token string) (string, bool, error) {
	colon := strings.Index(token, ":")
	if colon == -1 {
		return "", false, nil
	}
	agent := token[:colon]
	provided := token[colon+1:]

	secret, err := m.Config.Get("mcp_oauth_bearer_secret")
	if err != nil {
		return "", false, err
	}
	expected := HMACToken(secret, agent)[colon+1:]
	if len(provided) != len(expected) {
		return "", false, nil
	}
	if subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
		return "", false, nil
	}
	return agent, true, nil
}

func (m *MCPAuth) permissions(ctx context.Context, actorID int) ([]string, error) {
	rows, err := m.DB.Query(ctx,
		"SELECT p.name FROM agent_permissions ap JOIN permissions p ON p.id = ap.permission_id WHERE ap.actor_id = $1",
		actorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// activityEvent is the agent_activity payload, in Node's key order.
type activityEvent struct {
	Agent  string `json:"agent"`
	Active bool   `json:"active"`
}

// heartbeat stamps last_seen, keeps a running activity spinner alive by
// moving active_since forward when it is set, and stamps the account's first
// MCP connection. When the spinner was running it re-broadcasts it.
func (m *MCPAuth) heartbeat(actorID int, agent string, since *string) {
	db.Background(func(ctx context.Context) {
		var wasActive bool
		err := m.DB.QueryRow(ctx,
			`UPDATE actors
         SET last_seen = NOW(),
             active_since = CASE WHEN active_since IS NOT NULL THEN NOW() ELSE active_since END,
             `+signup.ConnectedSQL+`
         WHERE id = $1
         RETURNING (active_since IS NOT NULL) AS was_active`,
			actorID, since).Scan(&wasActive)
		if err == nil && wasActive {
			m.Events.Broadcast("agent_activity", activityEvent{Agent: agent, Active: true})
		}
	})
}

// Middleware wraps the /mcp route. A database or config failure is the
// last-resort 500, as the Node middleware's rejected promise was.
func (m *MCPAuth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var token string
		header := r.Header.Get("Authorization")
		if strings.HasPrefix(header, "Bearer ") {
			token = header[len("Bearer "):]
		} else if q, ok := httpapi.Query(r).String("token"); ok && q != "" {
			// req.query.token as Express parsed it: a repeated name is an
			// array, which is not a token.
			token = q
		} else {
			m.challenge(w, r)
			httpapi.WriteJSON(w, http.StatusUnauthorized, oauthError{"unauthorized", "Missing or invalid Authorization header"})
			return
		}

		ctx := r.Context()
		st := httpapi.StateOf(r)

		agent, ok, err := m.verifyHMAC(token)
		if err != nil {
			httpapi.WriteUnhandledError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if ok {
			actor, found, err := m.Actors.ResolveByName(ctx, agent)
			if err != nil {
				httpapi.WriteUnhandledError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if !found {
				httpapi.WriteJSON(w, http.StatusUnauthorized, oauthError{"invalid_token", "Agent not found in actors"})
				return
			}
			perms, err := m.permissions(ctx, actor.ID)
			if err != nil {
				httpapi.WriteUnhandledError(w, http.StatusInternalServerError, err.Error())
				return
			}
			since, err := signup.TrackingSince(m.Config)
			if err != nil {
				httpapi.WriteUnhandledError(w, http.StatusInternalServerError, err.Error())
				return
			}
			st.MCPAgent = agent
			st.MCPActorID = actor.ID
			st.MCPPermissions = perms
			m.heartbeat(actor.ID, agent, since)
			next.ServeHTTP(w, r)
			return
		}

		// API key path. Any failure here, a missing signup_funnel_since row
		// included, falls through to the 401: in Node this whole branch sat
		// inside one try/catch.
		key, err := m.APIKeys.Find(ctx, token, 0)
		if err == nil && key != nil {
			perms, err := m.permissions(ctx, key.ActorID)
			since, sinceErr := signup.TrackingSince(m.Config)
			if err == nil && sinceErr == nil {
				st.MCPAgent = key.Agent
				st.MCPActorID = key.ActorID
				st.MCPPermissions = perms
				m.heartbeat(key.ActorID, key.Agent, since)
				next.ServeHTTP(w, r)
				return
			}
		}

		// No discovery hint for a failed raw API key: the hint makes Claude
		// Code start OAuth discovery and cache an empty token, which breaks
		// API-key auth for that client for good.
		if !LooksLikeAPIKey(token) {
			m.challenge(w, r)
		}
		httpapi.WriteJSON(w, http.StatusUnauthorized, oauthError{"invalid_token", "Invalid token or API key"})
	})
}
