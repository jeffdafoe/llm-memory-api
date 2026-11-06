// Package agent is Node's routes/agent.js: the /v1/agent routes agents call
// for their own session, profile, roster and startup instructions, and the
// two that call the virtual-agent service (/agent/tick, /agent/rate-limit).
package agent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/diceware"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/events"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sanitize"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/signup"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/visibility"
)

// sessionTTL is how long an agent login session lasts.
const sessionTTL = 24 * time.Hour

// credentialWriteTimeout bounds the credential writes of logout and rotate,
// which run on after the client hangs up.
const credentialWriteTimeout = 30 * time.Second

// Routes are the /v1/agent routes.
type Routes struct {
	DB          db.DB
	Config      *config.Store
	Log         *logger.Logger
	Router      *httpapi.Router
	Events      events.Broadcaster
	Sessions    *auth.Sessions
	SessionAuth TokenCache
	Notes       *notes.Service
	Namespaces  *permissions.Namespaces
	Visibility  *visibility.Visibility
	// Agents is the virtual-agent service behind /agent/tick and
	// /agent/rate-limit.
	Agents VirtualAgents
	// Now is the clock for session expiry and the prune cutoff; nil is
	// time.Now.
	Now func() time.Time
	// Location is the zone memory-sync reads a local mtime ("...T10:00"
	// with no offset) in, as V8 used the process's zone; nil is time.Local.
	Location *time.Location
}

// TokenCache is the /v1 auth middleware's token cache (auth.SessionAuth),
// which logout and rotate clear.
type TokenCache interface {
	Forget(token string)
	ForgetAgent(agent string)
}

// Register adds the routes to the /v1 mux.
func (rt *Routes) Register(mux *httpapi.RawMux) {
	route := func(path, label string, fn httpapi.HandlerFunc) {
		mux.Handle(http.MethodPost, "/agent/"+path, rt.Router.Route("agent", label, fn))
	}
	route("login", "login", rt.login)
	route("logout", "logout", rt.logout)
	route("rotate", "rotate", rt.rotate)
	route("heartbeat", "heartbeat", rt.heartbeat)
	route("status", "status", rt.status)
	route("expertise", "expertise", rt.expertise)
	route("profile", "profile", rt.profile)
	route("activity/start", "activity-start", rt.activityStart)
	route("activity/stop", "activity-stop", rt.activityStop)
	route("signup-event", "signup-event", rt.signupEvent)
	route("instructions/read", "instructions-read", rt.instructionsRead)
	route("instructions/save", "instructions-save", rt.instructionsSave)
	route("memory/sync", "memory-sync", rt.memorySync)
	route("sync-mappings", "sync-mappings", rt.syncMappings)
	route("config", "agent-config", rt.agentConfig)
	route("rate-limit", "rate-limit", rt.rateLimit)
	route("tick", "tick", rt.tick)
}

func (rt *Routes) now() time.Time {
	if rt.Now != nil {
		return rt.Now()
	}
	return time.Now()
}

func (rt *Routes) logAgent(action string, details logger.Fields) {
	rt.Log.Log("agent", action, details)
}

// agentField is the "agent" log field, left out when there is no agent:
// JSON.stringify drops a key whose value is undefined.
func agentField(agent string) logger.Fields {
	if agent == "" {
		return nil
	}
	return logger.Fields{{Key: "agent", Value: agent}}
}

// requireAgent answers 401 for a caller that is not an agent (an admin web
// session). It reports whether the route may go on.
func requireAgent(w http.ResponseWriter, r *http.Request) (string, bool) {
	agent := httpapi.StateOf(r).AuthenticatedAgent
	if agent == "" {
		httpapi.WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "Agent session required")
		return "", false
	}
	return agent, true
}

// noBodyRead is V8's TypeError for `req.body.<field>` with no JSON body.
func noBodyRead(field string) error {
	return errors.New("Cannot read properties of undefined (reading '" + field + "')")
}

// dummySalt is hashed against when the agent is unknown, so that path takes
// as long as a wrong passphrase and the response time does not tell which
// agents exist.
var dummySalt = auth.GenerateSalt()

// newSessionToken is 48 random bytes in URL-safe base64 (64 characters).
func newSessionToken() string {
	b := make([]byte, 48)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

type loginResponse struct {
	Agent        string `json:"agent"`
	SessionToken string `json:"session_token"`
	ExpiresAt    string `json:"expires_at"`
}

// login checks a passphrase and opens a 24-hour API session. No token is
// needed; the passphrase is the credential. Expired sessions are removed by
// the daily db-cleanup cron, not here.
func (rt *Routes) login(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("passphrase")
	}
	agent, agentIsString := f.String("agent")
	agentGiven := f.Truthy("agent")
	if agentIsString {
		agent = sanitize.AgentName(agent)
		agentGiven = agent != ""
	}
	if !agentGiven || !f.Truthy("passphrase") {
		return httpapi.BadRequest(w, "Required fields: agent, passphrase")
	}
	if !agentIsString {
		return httpapi.NotString("agent")
	}
	passphrase, err := f.RequireString("passphrase")
	if err != nil {
		return err
	}
	subsystem, err := f.OptionalString("subsystem")
	if err != nil {
		return err
	}
	subsystem = sanitize.Identifier(subsystem)

	// The join to agent_configuration makes sure the actor really is an
	// agent: a web-only account could have a token_hash too.
	var (
		actorID      int
		name, status *string
		hash, salt   *string
	)
	err = rt.DB.QueryRow(r.Context(), `SELECT a.id AS actor_id, a.name AS agent, a.token_hash, a.token_salt, a.status
         FROM actors a
         JOIN agent_configuration agc ON agc.actor_id = a.id
         WHERE a.name = $1 AND a.token_hash IS NOT NULL`, agent).Scan(&actorID, &name, &hash, &salt, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		auth.Hash(passphrase, dummySalt)
		rt.logAgent("login-failed", logger.Fields{{Key: "agent", Value: agent}})
		httpapi.WriteError(w, http.StatusForbidden, "INVALID_CREDENTIALS", "Invalid agent or passphrase")
		return nil
	}
	if err != nil {
		return err
	}
	if salt == nil {
		return auth.ErrNullSalt
	}
	if !auth.VerifyLoose(passphrase, *salt, *hash) {
		rt.logAgent("login-failed", logger.Fields{{Key: "agent", Value: agent}})
		httpapi.WriteError(w, http.StatusForbidden, "INVALID_CREDENTIALS", "Invalid agent or passphrase")
		return nil
	}

	// The token is stored as a PBKDF2 hash (at rest) and a SHA-256 lookup
	// hash (an indexed lookup, so a request verifies one row).
	token := newSessionToken()
	sessionSalt := auth.GenerateSalt()
	sessionHash, err := auth.Hash(token, sessionSalt)
	if err != nil {
		return err
	}
	expiresAt := rt.now().Add(sessionTTL).Truncate(time.Millisecond)
	var subsystemValue *string
	if subsystem != "" {
		subsystemValue = &subsystem
	}
	if _, err := rt.DB.Exec(r.Context(),
		"INSERT INTO sessions (actor_id, token_hash, token_salt, token_lookup_hash, kind, expires_at, subsystem) VALUES ($1, $2, $3, $4, $5, $6, $7)",
		actorID, sessionHash, sessionSalt, auth.TokenLookupHash(token), auth.SessionKindAPI, expiresAt, subsystemValue); err != nil {
		return err
	}
	if _, err := rt.DB.Exec(r.Context(), "UPDATE actors SET last_seen = NOW() WHERE id = $1", actorID); err != nil {
		return err
	}

	rt.logAgent("login", logger.Fields{{Key: "agent", Value: agent}, {Key: "subsystem", Value: subsystemValue}})
	return httpapi.WriteJSON(w, http.StatusOK, loginResponse{Agent: agent, SessionToken: token, ExpiresAt: jsutil.DateISO(expiresAt)})
}

type messageResponse struct {
	Agent   string `json:"agent,omitempty"`
	Message string `json:"message"`
}

// logout deletes the API session of the token the request used.
func (rt *Routes) logout(w http.ResponseWriter, r *http.Request) error {
	st := httpapi.StateOf(r)
	agent := st.AuthenticatedAgent
	token := strings.Replace(r.Header.Get("Authorization"), "Bearer ", "", 1)

	// A client that hangs up cannot stop the delete halfway, and the cached
	// token is dropped even when the delete fails (by the defer).
	defer rt.SessionAuth.Forget(token)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), credentialWriteTimeout)
	defer cancel()
	deleted, err := rt.Sessions.DeleteByToken(ctx, token, auth.SessionKindAPI, st.ActorID)
	if err != nil {
		return err
	}
	rt.SessionAuth.Forget(token)

	rt.logAgent("logout", append(agentField(agent), logger.Field{Key: "session_deleted", Value: deleted}))
	return httpapi.WriteJSON(w, http.StatusOK, messageResponse{Agent: agent, Message: "Logged out"})
}

type rotateResponse struct {
	Agent      string `json:"agent,omitempty"`
	Passphrase string `json:"passphrase"`
	Message    string `json:"message"`
}

// rotate replaces the agent's passphrase and ends all its API sessions.
// The session proves who is asking; the current passphrase confirms intent.
func (rt *Routes) rotate(w http.ResponseWriter, r *http.Request) error {
	st := httpapi.StateOf(r)
	agent := st.AuthenticatedAgent
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("current_passphrase")
	}
	if !f.Truthy("current_passphrase") {
		return httpapi.BadRequest(w, "Required field: current_passphrase")
	}
	current, err := f.RequireString("current_passphrase")
	if err != nil {
		return err
	}

	var hash, salt *string
	if err := rt.DB.QueryRow(r.Context(), "SELECT token_hash, token_salt FROM actors WHERE id = $1", st.ActorID).Scan(&hash, &salt); err != nil {
		return err
	}
	if salt == nil {
		return auth.ErrNullSalt
	}
	stored := ""
	if hash != nil {
		stored = *hash
	}
	if !auth.VerifyLoose(current, *salt, stored) {
		httpapi.WriteError(w, http.StatusForbidden, "INVALID_PASSPHRASE", "Current passphrase does not match")
		return nil
	}

	passphrase := diceware.Passphrase()
	newSalt := auth.GenerateSalt()
	newHash, err := auth.Hash(passphrase, newSalt)
	if err != nil {
		return err
	}
	// The new credentials and the end of the old sessions commit together,
	// and a client that hangs up cannot cancel them halfway (Node's queries
	// ran on after a disconnect). The cached tokens are dropped on every
	// path from here, even when the outcome is unknown: before the answer on
	// success (as in Node), by the defer otherwise.
	if agent != "" {
		defer rt.SessionAuth.ForgetAgent(agent)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), credentialWriteTimeout)
	defer cancel()
	tx, err := rt.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		"UPDATE actors SET token_hash = $1, token_salt = $2, passphrase_rotated_at = NOW() WHERE id = $3",
		newHash, newSalt, st.ActorID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM sessions WHERE actor_id = $1 AND kind = $2", st.ActorID, auth.SessionKindAPI); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if agent != "" {
		rt.SessionAuth.ForgetAgent(agent)
	}

	rt.logAgent("rotate", agentField(agent))
	return httpapi.WriteJSON(w, http.StatusOK, rotateResponse{
		Agent:      agent,
		Passphrase: passphrase,
		Message:    "Passphrase rotated. Save this — it will not be shown again. All sessions have been invalidated.",
	})
}

type heartbeatResponse struct {
	Agent    string  `json:"agent"`
	LastSeen *string `json:"last_seen"`
}

// isoTime is a timestamptz as node-postgres put it in JSON.
func isoTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := jsutil.DateISO(*t)
	return &s
}

// heartbeat stamps last_seen. MCP servers call it every two minutes; the
// /v1 heartbeat middleware already stamps every request, so this is the
// fallback for idle agents.
func (rt *Routes) heartbeat(w http.ResponseWriter, r *http.Request) error {
	agent, ok := requireAgent(w, r)
	if !ok {
		return nil
	}
	var lastSeen *time.Time
	err := rt.DB.QueryRow(r.Context(), "UPDATE actors SET last_seen = NOW() WHERE id = $1 RETURNING last_seen", httpapi.StateOf(r).ActorID).Scan(&lastSeen)
	if errors.Is(err, pgx.ErrNoRows) {
		httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", `Agent "`+agent+`" is not registered`)
		return nil
	}
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, heartbeatResponse{Agent: agent, LastSeen: isoTime(lastSeen)})
}

type rosterEntry struct {
	Agent       string          `json:"agent"`
	Status      *string         `json:"status"`
	LastSeen    *string         `json:"last_seen"`
	Expertise   json.RawMessage `json:"expertise"`
	Provider    *string         `json:"provider"`
	Model       *string         `json:"model"`
	ActiveSince *string         `json:"active_since"`
	Subsystems  []string        `json:"subsystems"`
	UnreadChat  int             `json:"unread_chat"`
	UnreadMail  int             `json:"unread_mail"`
}

// orNull is `value || null` for a text column.
func orNull(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}

// expertiseOrEmpty is `row.expertise || []` for the jsonb column: a falsy
// JSON value becomes [], anything else is written as JSON.stringify would.
func expertiseOrEmpty(raw []byte) (json.RawMessage, error) {
	if raw == nil {
		return json.RawMessage("[]"), nil
	}
	v, err := jsutil.Parse(raw)
	if err != nil {
		return nil, err
	}
	if !jsutil.Truthy(v) {
		return json.RawMessage("[]"), nil
	}
	s, err := jsutil.Stringify(raw)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(s), nil
}

// status is the agent roster: every agent the caller may see, its status,
// and the caller's unread chat (outside discussions) and mail from it. An
// admin web session has no inbox, so its counts are 0.
func (rt *Routes) status(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	actorID := httpapi.StateOf(r).ActorID

	visible, err := rt.Visibility.VisibleActorIDs(ctx, actorID)
	if err != nil {
		return err
	}

	sql := `SELECT
            a.agent,
            a.status,
            a.last_seen,
            a.expertise,
            a.provider,
            a.model,
            a.virtual,
            a.active_since,
            COALESCE(c.unread_count, 0)::int AS unread_chat,
            COALESCE(m.unread_count, 0)::int AS unread_mail
        FROM agent_status a
        LEFT JOIN (
            SELECT fa.name AS from_agent, COUNT(*) AS unread_count
            FROM chat_messages cm
            JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
            JOIN actors fa ON fa.id = cmt.from_actor_id
            WHERE cm.to_actor_id = $1 AND cm.acked_at IS NULL AND cm.deleted_at IS NULL AND cmt.discussion_id IS NULL
            GROUP BY fa.name
        ) c ON c.from_agent = a.agent
        LEFT JOIN (
            SELECT fa.name AS from_agent, COUNT(*) AS unread_count
            FROM mail ml
            JOIN actors fa ON fa.id = ml.from_actor_id
            WHERE ml.to_actor_id = $1 AND ml.acked_at IS NULL AND ml.deleted_at IS NULL
            GROUP BY fa.name
        ) m ON m.from_agent = a.agent`
	params := []any{actorID}
	if !visible.All {
		sql += " WHERE a.actor_id = ANY($2)"
		params = append(params, visible.IDs)
	}
	sql += " ORDER BY a.agent"

	rows, err := rt.DB.Query(ctx, sql, params...)
	if err != nil {
		return err
	}
	type rosterRow struct {
		entry     rosterEntry
		expertise []byte
	}
	var roster []rosterRow
	for rows.Next() {
		var (
			row                   rosterRow
			lastSeen, activeSince *time.Time
			virtual               *bool
		)
		if err := rows.Scan(&row.entry.Agent, &row.entry.Status, &lastSeen, &row.expertise, &row.entry.Provider,
			&row.entry.Model, &virtual, &activeSince, &row.entry.UnreadChat, &row.entry.UnreadMail); err != nil {
			rows.Close()
			return err
		}
		row.entry.LastSeen, row.entry.ActiveSince = isoTime(lastSeen), isoTime(activeSince)
		roster = append(roster, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// The subsystems each agent has a live API session for, in name order.
	subsystems := map[string][]string{}
	sessionRows, err := rt.DB.Query(ctx, `SELECT ac.name AS agent, s.subsystem
        FROM sessions s
        JOIN actors ac ON ac.id = s.actor_id
        WHERE s.kind = $1 AND s.expires_at > NOW() AND s.subsystem IS NOT NULL
        ORDER BY ac.name, s.subsystem`, auth.SessionKindAPI)
	if err != nil {
		return err
	}
	for sessionRows.Next() {
		var agent, subsystem string
		if err := sessionRows.Scan(&agent, &subsystem); err != nil {
			sessionRows.Close()
			return err
		}
		list := subsystems[agent]
		seen := false
		for _, s := range list {
			if s == subsystem {
				seen = true
			}
		}
		if !seen {
			subsystems[agent] = append(list, subsystem)
		}
	}
	sessionRows.Close()
	if err := sessionRows.Err(); err != nil {
		return err
	}

	agents := make([]rosterEntry, 0, len(roster))
	for _, row := range roster {
		e := row.entry
		expertise, err := expertiseOrEmpty(row.expertise)
		if err != nil {
			return err
		}
		e.Expertise = expertise
		e.Provider, e.Model = orNull(e.Provider), orNull(e.Model)
		e.Subsystems = subsystems[e.Agent]
		if e.Subsystems == nil {
			e.Subsystems = []string{}
		}
		agents = append(agents, e)
	}
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Agents []rosterEntry `json:"agents"`
	}{agents})
}

type expertiseResponse struct {
	Agent     string   `json:"agent"`
	Expertise []string `json:"expertise"`
	Message   string   `json:"message"`
}

// expertise replaces the agent's expertise list. Entries that are not
// non-blank strings are dropped; the rest are trimmed and lowercased.
func (rt *Routes) expertise(w http.ResponseWriter, r *http.Request) error {
	agent, ok := requireAgent(w, r)
	if !ok {
		return nil
	}
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("expertise")
	}
	list, isArray := f.Array("expertise")
	if !isArray {
		return httpapi.BadRequest(w, "Required field: expertise (array of strings)")
	}
	cleaned := []string{}
	for _, raw := range list {
		if len(raw) == 0 || raw[0] != '"' {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			continue
		}
		s = jsutil.Trim(s)
		if s == "" {
			continue
		}
		cleaned = append(cleaned, sanitize.Content(jsutil.ToLower(s)))
	}
	text, err := jsonutil.Marshal(cleaned)
	if err != nil {
		return err
	}
	if _, err := rt.DB.Exec(r.Context(), "UPDATE actors SET expertise = $1 WHERE id = $2", string(text), httpapi.StateOf(r).ActorID); err != nil {
		return err
	}
	rt.logAgent("expertise_update", logger.Fields{{Key: "agent", Value: agent}, {Key: "expertise", Value: cleaned}})
	return httpapi.WriteJSON(w, http.StatusOK, expertiseResponse{Agent: agent, Expertise: cleaned, Message: "Expertise updated"})
}

// optionalIdentifier is a profile field: given (present in the body, null
// included) and its sanitized value (nil for null).
type optionalIdentifier struct {
	given bool
	value *string
}

func identifierField(f httpapi.Fields, name string) (optionalIdentifier, error) {
	raw, present := f[name]
	if !present {
		return optionalIdentifier{}, nil
	}
	if string(raw) == "null" {
		return optionalIdentifier{given: true}, nil
	}
	s, isString := f.String(name)
	if !isString {
		return optionalIdentifier{}, httpapi.NotString(name)
	}
	s = sanitize.Identifier(s)
	return optionalIdentifier{given: true, value: &s}, nil
}

// profile sets the agent's provider and/or model. A field sent as null
// clears it.
func (rt *Routes) profile(w http.ResponseWriter, r *http.Request) error {
	agent, ok := requireAgent(w, r)
	if !ok {
		return nil
	}
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBodyRead("provider")
	}
	_, providerGiven := f["provider"]
	_, modelGiven := f["model"]
	if !providerGiven && !modelGiven {
		return httpapi.BadRequest(w, "At least one field required: provider, model")
	}
	provider, err := identifierField(f, "provider")
	if err != nil {
		return err
	}
	model, err := identifierField(f, "model")
	if err != nil {
		return err
	}

	var sets []string
	var vals []any
	if provider.given {
		sets = append(sets, "provider = $"+itoa(len(vals)+1))
		vals = append(vals, provider.value)
	}
	if model.given {
		sets = append(sets, "model = $"+itoa(len(vals)+1))
		vals = append(vals, model.value)
	}
	vals = append(vals, httpapi.StateOf(r).ActorID)
	if _, err := rt.DB.Exec(r.Context(),
		"UPDATE agent_configuration SET "+strings.Join(sets, ", ")+" WHERE actor_id = $"+itoa(len(vals)), vals...); err != nil {
		return err
	}

	details := logger.Fields{{Key: "agent", Value: agent}}
	resp := jsutil.NewObject()
	resp.Set("agent", agent)
	if provider.given {
		details = append(details, logger.Field{Key: "provider", Value: provider.value})
		resp.Set("provider", jsValue(provider.value))
	}
	if model.given {
		details = append(details, logger.Field{Key: "model", Value: model.value})
		resp.Set("model", jsValue(model.value))
	}
	resp.Set("message", "Profile updated")
	rt.logAgent("profile_update", details)
	return writeObject(w, resp)
}

type activityResponse struct {
	Agent   string `json:"agent"`
	Active  bool   `json:"active"`
	Message string `json:"message"`
}

type activityEvent struct {
	Agent  string `json:"agent"`
	Active bool   `json:"active"`
}

func (rt *Routes) setActivity(w http.ResponseWriter, r *http.Request, active bool) error {
	agent, ok := requireAgent(w, r)
	if !ok {
		return nil
	}
	sql, action, message := "UPDATE actors SET active_since = NOW() WHERE id = $1", "activity_start", "Activity started"
	if !active {
		sql, action, message = "UPDATE actors SET active_since = NULL WHERE id = $1", "activity_stop", "Activity stopped"
	}
	if _, err := rt.DB.Exec(r.Context(), sql, httpapi.StateOf(r).ActorID); err != nil {
		return err
	}
	rt.logAgent(action, logger.Fields{{Key: "agent", Value: agent}})
	rt.Events.Broadcast("agent_activity", activityEvent{Agent: agent, Active: active})
	return httpapi.WriteJSON(w, http.StatusOK, activityResponse{Agent: agent, Active: active, Message: message})
}

// activityStart turns on the agent's activity indicator.
func (rt *Routes) activityStart(w http.ResponseWriter, r *http.Request) error {
	return rt.setActivity(w, r, true)
}

// activityStop turns it off.
func (rt *Routes) activityStop(w http.ResponseWriter, r *http.Request) error {
	return rt.setActivity(w, r, false)
}

// signupEvent records what a new account did on the signup guide (LLM-734).
// The guide authenticates with the API key it was just handed.
func (rt *Routes) signupEvent(w http.ResponseWriter, r *http.Request) error {
	if _, ok := requireAgent(w, r); !ok {
		return nil
	}
	f, _ := httpapi.BodyFields(r)
	result, err := signup.RecordEvent(r.Context(), rt.DB, httpapi.StateOf(r).ActorID, f.Raw("event"), f.Raw("value"))
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, result)
}

type instructionsResponse struct {
	Agent        string  `json:"agent"`
	Instructions *string `json:"instructions"`
}

// instructionsRead is the agent's startup instructions, with the dream
// bootstrap appended when dreaming is on, and then its context/soul note
// when one exists.
func (rt *Routes) instructionsRead(w http.ResponseWriter, r *http.Request) error {
	agent, ok := requireAgent(w, r)
	if !ok {
		return nil
	}
	ctx := r.Context()
	// An agent with no agent_configuration row reads as one with nothing set.
	config, _, err := LoadInstructions(ctx, rt.DB, httpapi.StateOf(r).ActorID)
	if err != nil {
		return err
	}
	var instructions *string
	if config.Startup != nil && *config.Startup != "" {
		s := *config.Startup
		instructions = &s
	}

	if config.Dreams() {
		bootstrap, err := rt.Config.Get("dream_bootstrap")
		if err != nil {
			return err
		}
		if bootstrap != "" {
			instructions = appendText(instructions, bootstrap)
		}
	}

	// The living soul document dream processing maintains. A missing note
	// (or any read failure) is skipped.
	if soul, err := rt.Notes.Read(ctx, agent, "context/soul"); err == nil && soul.Content != "" {
		instructions = appendText(instructions, soul.Content)
	}
	return httpapi.WriteJSON(w, http.StatusOK, instructionsResponse{Agent: agent, Instructions: instructions})
}

// appendText adds a blank line and text; nil instructions count as empty.
func appendText(instructions *string, text string) *string {
	s := ""
	if instructions != nil {
		s = *instructions
	}
	s += "\n\n" + text
	return &s
}

type instructionsSaveResponse struct {
	Agent   string `json:"agent"`
	Message string `json:"message"`
	Length  int    `json:"length"`
}

// instructionsSave replaces the agent's startup instructions. null clears
// them.
func (rt *Routes) instructionsSave(w http.ResponseWriter, r *http.Request) error {
	agent, ok := requireAgent(w, r)
	if !ok {
		return nil
	}
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBodyRead("content")
	}
	raw, present := f["content"]
	if !present {
		return httpapi.BadRequest(w, "Required field: content")
	}
	var content *string
	if string(raw) != "null" {
		s, isString := f.String("content")
		if !isString {
			return httpapi.NotString("content")
		}
		s = sanitize.Content(s)
		content = &s
	}
	if err := SaveInstructions(r.Context(), rt.DB, httpapi.StateOf(r).ActorID, content); err != nil {
		return err
	}
	length := 0
	if content != nil {
		length = jsutil.UTF16Len(*content)
	}
	return httpapi.WriteJSON(w, http.StatusOK, instructionsSaveResponse{Agent: agent, Message: "Instructions saved", Length: length})
}

type syncMapping struct {
	ID        int     `json:"id"`
	Namespace string  `json:"namespace"`
	Slug      string  `json:"slug"`
	LocalPath string  `json:"local_path"`
	CreatedAt *string `json:"created_at"`
}

// syncMappings lists the agent's note sync mappings (note prefix to local
// path) and the global exclude list, for the sync script.
func (rt *Routes) syncMappings(w http.ResponseWriter, r *http.Request) error {
	agent, ok := requireAgent(w, r)
	if !ok {
		return nil
	}
	rows, err := rt.DB.Query(r.Context(), `
        SELECT ns.id, ns.namespace, ns.slug, ns.local_path, ns.created_at
        FROM note_synchronization ns
        JOIN actors a ON a.id = ns.actor_id
        WHERE a.name = $1
        ORDER BY ns.namespace, ns.slug
    `, agent)
	if err != nil {
		return err
	}
	mappings := []syncMapping{}
	for rows.Next() {
		var m syncMapping
		var created *time.Time
		if err := rows.Scan(&m.ID, &m.Namespace, &m.Slug, &m.LocalPath, &created); err != nil {
			rows.Close()
			return err
		}
		m.CreatedAt = isoTime(created)
		mappings = append(mappings, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// A missing key means no exclusions.
	exclude := []string{}
	if raw, ok := rt.Config.Lookup("sync_exclude_slugs"); ok && raw != "" {
		for _, s := range strings.Split(raw, ",") {
			if s = jsutil.Trim(s); s != "" {
				exclude = append(exclude, s)
			}
		}
	}
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Mappings     []syncMapping `json:"mappings"`
		ExcludeSlugs []string      `json:"exclude_slugs"`
	}{mappings, exclude})
}

// agentConfigKeys are the config values any caller may read: operational
// parameters tools such as the discuss binary need at run time.
var agentConfigKeys = []string{
	"discussion_end_timeout_realtime",
	"discussion_maxrounds",
}

// agentConfig returns the whitelisted config values, in the order the
// database returns them.
func (rt *Routes) agentConfig(w http.ResponseWriter, r *http.Request) error {
	rows, err := rt.DB.Query(r.Context(), "SELECT key, value FROM config WHERE key = ANY($1)", agentConfigKeys)
	if err != nil {
		return err
	}
	values := jsutil.NewObject()
	for rows.Next() {
		var key string
		var value *string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			return err
		}
		values.Set(key, jsValue(value))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	resp := jsutil.NewObject()
	resp.Set("config", values)
	return writeObject(w, resp)
}

// writeObject writes an ordered object as JSON.stringify would.
func writeObject(w http.ResponseWriter, o *jsutil.Object) error {
	return httpapi.WriteJSON(w, http.StatusOK, json.RawMessage(jsutil.Marshal(o)))
}

// jsValue is a nullable text value as an ordered-object member.
func jsValue(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func itoa(n int) string {
	return jsutil.NumberString(float64(n))
}
