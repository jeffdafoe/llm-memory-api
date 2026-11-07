// Package registration is Node's routes/registration.js: the public signup
// endpoints (no auth). An account is created from an invite code, or without
// one when open registration is on.
package registration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/diceware"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/frontmatter"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/mail"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgbind"
)

// namePattern is the rule for a new agent name (after trim and lowercase).
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,30}$`)

const nameRule = "Name must start with a letter, 2-31 chars, only lowercase letters, numbers, hyphens, underscores."

// Moderator reviews a proposed name (virtualagent.Moderator).
type Moderator interface {
	ModerateActorName(ctx context.Context, name string) (approved bool, reason string)
}

// Routes are the registration endpoints.
type Routes struct {
	DB        db.DB
	Config    *config.Store
	Actors    *actors.Cache
	Log       *logger.Logger
	Mail      *mail.Service
	Notes     *notes.Service
	Moderator Moderator
	// Now is the clock for invite expiry; nil is time.Now.
	Now func() time.Time
}

// Register adds the routes to the top-level mux.
func (rt *Routes) Register(m *httpapi.RawMux) {
	m.Handle(http.MethodGet, "/api/registration-mode", http.HandlerFunc(rt.mode))
	m.Handle(http.MethodPost, "/api/check-name", http.HandlerFunc(rt.checkName))
	m.Handle(http.MethodPost, "/api/register", http.HandlerFunc(rt.register))
}

func (rt *Routes) now() time.Time {
	if rt.Now != nil {
		return rt.Now()
	}
	return time.Now()
}

// unhandled is an error the Node handler threw: Express's error handler
// answered it, and nothing went to error_log.
func (rt *Routes) unhandled(w http.ResponseWriter, err error) {
	rt.Log.Errorf("Unhandled error: %v", err)
	httpapi.WriteUnhandledError(w, http.StatusInternalServerError, err.Error())
}

type errorBody struct {
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, message string) {
	httpapi.WriteJSON(w, status, errorBody{message})
}

func (rt *Routes) openRegistration() (bool, error) {
	v, err := rt.Config.Get("open_registration")
	return v == "true", err
}

// mode answers whether open registration is on.
func (rt *Routes) mode(w http.ResponseWriter, r *http.Request) {
	open, err := rt.openRegistration()
	if err != nil {
		rt.unhandled(w, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Open bool `json:"open"`
	}{open})
}

// notAFunction is V8's TypeError for name.trim() on a value that is not a
// string.
func notAFunction(expr string) error {
	return errors.New(expr + " is not a function")
}

// checkName answers whether a name is free.
func (rt *Routes) checkName(w http.ResponseWriter, r *http.Request) {
	body := httpapi.JSONParams(r)
	if body == nil {
		rt.unhandled(w, httpapi.NoBody("name"))
		return
	}
	if !jsutil.Truthy(body["name"]) {
		writeError(w, http.StatusBadRequest, "Name is required")
		return
	}
	name, ok := body["name"].(string)
	if !ok {
		rt.unhandled(w, notAFunction("name.trim"))
		return
	}
	if jsutil.Trim(name) == "" {
		writeError(w, http.StatusBadRequest, "Name is required")
		return
	}
	agentName := jsutil.ToLower(jsutil.Trim(name))
	if !namePattern.MatchString(agentName) {
		httpapi.WriteJSON(w, http.StatusOK, actors.Availability{Reason: nameRule})
		return
	}
	result, err := rt.Actors.CheckNameAvailability(r.Context(), rt.DB, agentName)
	if err != nil {
		rt.unhandled(w, err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, result)
}

// realmFromHost maps the request host to a realm through the realm_host_map
// config (a JSON object); anything else is the llm-memory realm.
func (rt *Routes) realmFromHost(r *http.Request) string {
	host := strings.Join(r.Header.Values("X-Forwarded-Host"), ", ")
	if host == "" {
		host = r.Host
	}
	host, _, _ = strings.Cut(host, ":")
	raw, err := rt.Config.Get("realm_host_map")
	if err != nil {
		return "llm-memory"
	}
	if raw == "" {
		raw = "{}"
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &m) != nil {
		return "llm-memory"
	}
	v, ok := m[host]
	if !ok {
		return "llm-memory"
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		if s != "" {
			return s
		}
		return "llm-memory"
	}
	var decoded any
	dec := json.NewDecoder(strings.NewReader(string(v)))
	dec.UseNumber()
	if dec.Decode(&decoded) != nil || !jsutil.Truthy(decoded) {
		return "llm-memory"
	}
	// A truthy non-string realm went to Postgres as node-postgres's text.
	switch t := pgbind.Value(v).(type) {
	case string:
		return t
	case jsutil.Number:
		return string(t)
	case bool:
		return strconv.FormatBool(t)
	}
	return "llm-memory"
}

type invite struct {
	id              int
	usedBy          *string
	expiresAt       *time.Time
	realm           *string
	accessRequestID *int
}

// inviteProblem says why an invite cannot be redeemed, or "". It is checked
// twice per registration: cheaply up front, then under the row lock.
func (rt *Routes) inviteProblem(inv *invite) string {
	if inv == nil {
		return "Invalid invite code"
	}
	if inv.usedBy != nil && *inv.usedBy != "" {
		return "This invite code has already been used"
	}
	if inv.expiresAt != nil && inv.expiresAt.Before(rt.now()) {
		return "This invite code has expired"
	}
	return ""
}

func realmOr(realm *string) string {
	if realm == nil || *realm == "" {
		return "llm-memory"
	}
	return *realm
}

// passwordLength is password.length as JavaScript compares it with a
// number: a string's UTF-16 length, an array's element count, an object's
// own length property; ok is false where the comparison is false either way
// (undefined or NaN).
func passwordLength(v any) (float64, bool) {
	switch x := v.(type) {
	case string:
		return float64(jsutil.UTF16Len(x)), true
	case []any:
		return float64(len(x)), true
	case map[string]any:
		l, has := x["length"]
		if !has {
			return 0, false
		}
		raw, _ := json.Marshal(l)
		n := jsutil.ToNumber(raw)
		return n, n == n
	}
	return 0, false
}

// register creates an agent: actor, configuration, MCP and dashboard
// grants, and an API key, then applies the welcome templates.
func (rt *Routes) register(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body := httpapi.JSONParams(r)
	if body == nil {
		rt.unhandled(w, httpapi.NoBody("code"))
		return
	}
	open, err := rt.openRegistration()
	if err != nil {
		rt.unhandled(w, err)
		return
	}
	if !jsutil.Truthy(body["name"]) {
		writeError(w, http.StatusBadRequest, "Agent name is required")
		return
	}
	if !jsutil.Truthy(body["code"]) && !open {
		writeError(w, http.StatusBadRequest, "Invite code is required (open registration is disabled)")
		return
	}

	minText, err := rt.Config.Get("minimum_password_length")
	if err != nil {
		rt.unhandled(w, err)
		return
	}
	minLen, ok := jsutil.ParseInt(minText)
	if !ok || minLen == 0 {
		minLen = 10
	}
	password := body["password"]
	length, comparable := passwordLength(password)
	if !jsutil.Truthy(password) || comparable && length < minLen {
		writeError(w, http.StatusBadRequest, "Password must be at least "+jsutil.NumberString(minLen)+" characters")
		return
	}
	dreamMode := "none"
	if s, ok := body["dream_mode"].(string); ok && (s == "none" || s == "companion" || s == "technical") {
		dreamMode = s
	}

	nameText, ok := body["name"].(string)
	if !ok {
		rt.unhandled(w, notAFunction("name.trim"))
		return
	}
	agentName := jsutil.ToLower(jsutil.Trim(nameText))
	if !namePattern.MatchString(agentName) {
		writeError(w, http.StatusBadRequest, nameRule)
		return
	}

	// Phase 1: checks with no transaction and no connection held. Bad codes,
	// taken names and moderation refusals are turned away before any PBKDF2
	// work. The invite check runs first, so a bad code never reaches the
	// moderation agent; the authoritative invite check is in phase 3.
	realm := ""
	var inviteID *int
	if jsutil.Truthy(body["code"]) {
		code, ok := body["code"].(string)
		if !ok {
			rt.unhandled(w, notAFunction("code.trim"))
			return
		}
		inv, err := rt.readInvite(ctx, rt.DB, "SELECT id, used_by, expires_at, realm, NULL::int FROM invite_codes WHERE code = $1", jsutil.Trim(code))
		if err != nil {
			rt.unhandled(w, err)
			return
		}
		problem := rt.inviteProblem(inv)
		// With open registration the code only links the account to its
		// access request, so a bad or stale one is dropped, not refused.
		if problem != "" && !open {
			writeError(w, http.StatusBadRequest, problem)
			return
		}
		if problem == "" {
			realm = realmOr(inv.realm)
			inviteID = &inv.id
		}
	}
	if inviteID == nil {
		realm = rt.realmFromHost(r)
	}

	availability, err := rt.Actors.CheckNameAvailability(ctx, rt.DB, agentName)
	if err != nil {
		rt.unhandled(w, err)
		return
	}
	if !availability.Available {
		writeError(w, http.StatusConflict, availability.Reason)
		return
	}
	if approved, reason := rt.Moderator.ModerateActorName(ctx, agentName); !approved {
		httpapi.WriteJSON(w, http.StatusBadRequest, struct {
			Error string `json:"error"`
			Field string `json:"field"`
		}{reason, "name"})
		return
	}

	// Phase 2: the hashing, with no connection or invite row lock held.
	phrase := diceware.Passphrase()
	salt := auth.GenerateSalt()
	phraseHash, err := auth.Hash(phrase, salt)
	if err != nil {
		rt.unhandled(w, err)
		return
	}
	passwordText, ok := password.(string)
	if !ok {
		rt.unhandled(w, errors.New(`The "password" argument must be of type string or an instance of ArrayBuffer, Buffer, TypedArray, or DataView. `+jsutil.Received(password)))
		return
	}
	passwordSalt := auth.GenerateSalt()
	passwordHash, err := auth.Hash(passwordText, passwordSalt)
	if err != nil {
		rt.unhandled(w, err)
		return
	}
	apiKey := auth.GenerateKey()
	keySalt := auth.GenerateSalt()
	keyHash, err := auth.Hash(apiKey, keySalt)
	if err != nil {
		rt.unhandled(w, err)
		return
	}

	// Phase 3: one short transaction of inserts.
	actorID, refusal, err := rt.createAccount(ctx, r, newAccount{
		name: agentName, open: open, inviteID: inviteID, realm: realm, dreamMode: dreamMode,
		phraseHash: phraseHash, salt: salt, passwordHash: passwordHash, passwordSalt: passwordSalt,
		keyHash: keyHash, keySalt: keySalt, keyLookup: auth.TokenLookupHash(apiKey),
	})
	if refusal != "" {
		writeError(w, http.StatusBadRequest, refusal)
		return
	}
	if err != nil {
		var pgErr *pgconn.PgError
		// A concurrent registration claimed the name between the phase 1
		// check and this insert.
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "actors_name_key" {
			writeError(w, http.StatusConflict, "That name was just taken. Please choose another.")
			return
		}
		rt.Log.Errorf("Registration error: %s", err)
		writeError(w, http.StatusInternalServerError, "Something went wrong")
		return
	}

	// The welcome templates run after the commit, so the agent exists even
	// when one fails.
	if err := rt.applyWelcome(ctx, agentName); err != nil {
		rt.Log.Errorf("Registration welcome template error: %s", err)
	}
	if err := rt.saveGettingStarted(ctx, agentName, phrase, apiKey, actorID); err != nil {
		rt.Log.Errorf("Registration welcome-note template error: %s", err)
	}

	httpapi.WriteJSON(w, http.StatusOK, struct {
		OK         bool   `json:"ok"`
		Agent      string `json:"agent"`
		Passphrase string `json:"passphrase"`
		APIKey     string `json:"api_key"`
		Message    string `json:"message"`
	}{true, agentName, phrase, apiKey, "Account created. Save your passphrase and API key — they will not be shown again."})
}

func (rt *Routes) readInvite(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, sql string, arg any) (*invite, error) {
	var inv invite
	err := q.QueryRow(ctx, sql, arg).Scan(&inv.id, &inv.usedBy, &inv.expiresAt, &inv.realm, &inv.accessRequestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

type newAccount struct {
	name, realm, dreamMode string
	open                   bool
	inviteID               *int
	phraseHash, salt       string
	passwordHash           string
	passwordSalt           string
	keyHash, keySalt       string
	keyLookup              string
}

// createAccount runs phase 3. The invite is locked and checked again, which
// closes the race two concurrent uses of one code would otherwise win
// together: the loser sees used_by set. refusal is an invite problem under
// invite-only registration (rolled back, a 400).
func (rt *Routes) createAccount(ctx context.Context, r *http.Request, a newAccount) (actorID int, refusal string, err error) {
	tx, err := rt.DB.Begin(ctx)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback(ctx)

	inviteID := a.inviteID
	realm := a.realm
	// The registrant's email comes from the access request the invite was
	// made for; code-less registration has none.
	var email *string
	if inviteID != nil {
		locked, err := rt.readInvite(ctx, tx, "SELECT id, used_by, expires_at, realm, access_request_id FROM invite_codes WHERE id = $1 FOR UPDATE", *inviteID)
		if err != nil {
			return 0, "", err
		}
		problem := rt.inviteProblem(locked)
		if problem != "" && !a.open {
			return 0, problem, nil
		}
		if problem != "" {
			// Lost the race for the code under open registration: register
			// without it, as phase 1 does for a bad code.
			inviteID = nil
			realm = rt.realmFromHost(r)
		} else {
			// The locked row's realm wins if an admin edited the invite
			// since phase 1.
			realm = realmOr(locked.realm)
			if locked.accessRequestID != nil {
				var e *string
				err := tx.QueryRow(ctx, "SELECT email FROM access_requests WHERE id = $1", *locked.accessRequestID).Scan(&e)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return 0, "", err
				}
				if err == nil {
					email = e
				}
			}
		}
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO actors (name, token_hash, token_salt, password_hash, password_salt, status, realms, email) VALUES ($1, $2, $3, $4, $5, 'active', $6, $7)`,
		a.name, a.phraseHash, a.salt, a.passwordHash, a.passwordSalt, []string{realm}, email); err != nil {
		return 0, "", err
	}
	// The agent owns itself.
	if _, err := tx.Exec(ctx, `UPDATE actors SET created_by = id WHERE name = $1`, a.name); err != nil {
		return 0, "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_configuration (actor_id, provider, model, dream_mode)
             VALUES ((SELECT id FROM actors WHERE name = $1), NULL, NULL, $2)`, a.name, a.dreamMode); err != nil {
		return 0, "", err
	}
	if inviteID != nil {
		if _, err := tx.Exec(ctx, `UPDATE invite_codes SET used_by = $1, used_at = NOW() WHERE id = $2`, a.name, *inviteID); err != nil {
			return 0, "", err
		}
	}
	if err := tx.QueryRow(ctx, "SELECT id FROM actors WHERE name = $1", a.name).Scan(&actorID); err != nil {
		return 0, "", err
	}
	// Every MCP permission (full tool access). Own-namespace access needs no
	// row.
	if _, err := tx.Exec(ctx, `INSERT INTO agent_permissions (actor_id, permission_id)
             SELECT $1, id FROM permissions`, actorID); err != nil {
		return 0, "", err
	}
	// Dashboard access: dashboard, agents, communications, notes.
	if _, err := tx.Exec(ctx, `INSERT INTO admin_permissions (actor_id, resource, action) VALUES
             ($1, 'dashboard', 'read'),
             ($1, 'agents', 'read'),
             ($1, 'agents', 'write'),
             ($1, 'comms', 'read'),
             ($1, 'notes', 'read'),
             ($1, 'notes', 'write')`, actorID); err != nil {
		return 0, "", err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_api_keys (actor_id, key_hash, key_salt, key_lookup_hash, label) VALUES ($1, $2, $3, $4, 'default')`,
		actorID, a.keyHash, a.keySalt, a.keyLookup); err != nil {
		return 0, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, "", err
	}
	return actorID, "", nil
}

// templateContent is the oldest template of a kind, or ok false when there
// is none.
func (rt *Routes) templateContent(ctx context.Context, kind string) (content *string, ok bool, err error) {
	err = rt.DB.QueryRow(ctx, "SELECT content FROM templates WHERE kind = $1 ORDER BY id LIMIT 1", kind).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return content, true, nil
}

// applyWelcome copies the welcome template into the agent's startup
// instructions and sends it as mail from system, as for admin-created
// agents.
func (rt *Routes) applyWelcome(ctx context.Context, agentName string) error {
	content, ok, err := rt.templateContent(ctx, "welcome")
	if err != nil || !ok {
		return err
	}
	if content == nil {
		return errors.New("Cannot read properties of null (reading 'match')")
	}
	front, tplBody := frontmatter.Parse(*content)
	mailBody := strings.ReplaceAll(tplBody, "{agent}", agentName)
	if _, err := rt.DB.Exec(ctx,
		"UPDATE agent_configuration SET startup_instructions = $1 WHERE actor_id = (SELECT id FROM actors WHERE name = $2)",
		mailBody, agentName); err != nil {
		return err
	}
	subject := front.Get("subject", "Welcome, {agent}")
	_, err = rt.Mail.Send(ctx, agentName, "system", strings.ReplaceAll(subject, "{agent}", agentName), mailBody, "")
	return err
}

// saveGettingStarted saves the welcome-note template in the new agent's
// namespace, with its credentials filled in.
func (rt *Routes) saveGettingStarted(ctx context.Context, agentName, phrase, apiKey string, actorID int) error {
	content, ok, err := rt.templateContent(ctx, "welcome-note")
	if err != nil || !ok {
		return err
	}
	if content == nil {
		return errors.New("Cannot read properties of null (reading 'match')")
	}
	front, tplBody := frontmatter.Parse(*content)
	noteBody := strings.ReplaceAll(tplBody, "{agent}", agentName)
	noteBody = strings.ReplaceAll(noteBody, "{passphrase}", phrase)
	noteBody = strings.ReplaceAll(noteBody, "{api_key}", apiKey)
	title := strings.ReplaceAll(front.Get("title", "Getting Started"), "{agent}", agentName)
	slug := front.Get("slug", "instructions/getting-started")
	// Node passed the new actor's id where saveNote takes a creator name, so
	// the creator is looked up by that number as a name.
	_, err = rt.Notes.Save(ctx, notes.SaveInput{
		Namespace: agentName, Title: title, Content: noteBody, Slug: slug, CreatedBy: strconv.Itoa(actorID),
	})
	return err
}
