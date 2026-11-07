package admin

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/diceware"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/frontmatter"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgbind"
)

// defaultRealm is the realm of an actor whose creator has none.
const defaultRealm = "llm-memory"

// maxEmailLength is the actors.email column width.
const maxEmailLength = 255

type actorCreated struct {
	ActorID          int    `json:"actor_id"`
	Name             string `json:"name"`
	Passphrase       string `json:"passphrase"`
	Virtual          bool   `json:"virtual"`
	UIAccess         bool   `json:"ui_access"`
	Status           string `json:"status"`
	WelcomeMailSent  bool   `json:"welcome_mail_sent"`
	WelcomeNoteSaved bool   `json:"welcome_note_saved"`
	Message          string `json:"message"`
	Warning          string `json:"warning,omitempty"`
}

// actorsCreate is POST /admin/actors/create: an actor with an agent
// configuration, optionally a dashboard password, and for an agent that is
// not virtual every MCP permission and the selected welcome templates.
//
// `virtual` is read two ways, as in Node: `=== true` sets the configuration
// flag, the status, the email default and the response; its truthiness
// decides the MCP grant, the welcome templates and the message.
func (rt *Routes) actorsCreate(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("name")
	}
	if !f.Truthy("name") {
		return httpapi.BadRequest(w, "Required field: name")
	}
	name, err := f.RequireString("name")
	if err != nil {
		return err
	}
	if jsutil.Trim(name) == "" {
		return httpapi.BadRequest(w, "Required field: name")
	}
	actorName := jsutil.ToLower(jsutil.Trim(name))

	uiAccess := f.Truthy("ui_access")
	password := ""
	if uiAccess {
		minText, err := rt.Config.Get("minimum_password_length")
		if err != nil {
			return err
		}
		minLen, ok := jsutil.ParseInt(minText)
		if !ok || minLen == 0 {
			minLen = 10
		}
		tooShort := "Password must be at least " + jsutil.NumberString(minLen) + " characters"
		if !f.Truthy("password") {
			return httpapi.BadRequest(w, tooShort)
		}
		if password, err = f.RequireString("password"); err != nil {
			return err
		}
		if float64(jsutil.UTF16Len(password)) < minLen {
			return httpapi.BadRequest(w, tooShort)
		}
	}

	notNull := func(name string) bool {
		raw, ok := f[name]
		return ok && string(raw) != "null"
	}
	var maxTokens any
	if notNull("max_tokens") {
		n, ok := SafeInt(f.Raw("max_tokens"))
		if !ok || n < 1 {
			return httpapi.BadRequest(w, "max_tokens must be a positive integer")
		}
		maxTokens = n
	}
	var temperature any
	if notNull("temperature") {
		t, err := parseFloatValue(f.Raw("temperature"))
		if err != nil {
			return err
		}
		if math.IsNaN(t) || t < 0 || t > 2 {
			return httpapi.BadRequest(w, "temperature must be a number between 0 and 2")
		}
		temperature = jsutil.Num(t)
	}
	email, emailIsString := f.String("email")
	if emailIsString && jsutil.UTF16Len(jsutil.Trim(email)) > maxEmailLength {
		return httpapi.BadRequest(w, "email must be 255 characters or less")
	}
	availability, err := rt.Actors.CheckNameAvailability(r.Context(), rt.DB, actorName)
	if err != nil {
		return err
	}
	if !availability.Available {
		httpapi.WriteError(w, http.StatusConflict, "ALREADY_EXISTS", availability.Reason)
		return nil
	}
	if approved, reason := rt.Moderator.ModerateActorName(r.Context(), actorName); !approved {
		httpapi.WriteError(w, http.StatusBadRequest, "NAME_REJECTED", reason)
		return nil
	}

	// Every actor gets a passphrase for API auth.
	phrase := diceware.Passphrase()
	phraseSalt := auth.GenerateSalt()
	phraseHash, err := auth.Hash(phrase, phraseSalt)
	if err != nil {
		return err
	}
	var passwordHash, passwordSalt any
	if uiAccess {
		salt := auth.GenerateSalt()
		hash, err := auth.Hash(password, salt)
		if err != nil {
			return err
		}
		passwordHash, passwordSalt = hash, salt
	}

	virtualTrue := string(f.Raw("virtual")) == "true"
	virtualTruthy := f.Truthy("virtual")
	configuration := any(nil)
	if f.Truthy("configuration") {
		conf, err := configurationText(f.Raw("configuration"))
		if err != nil {
			return err
		}
		configuration = conf
	}
	dreamMode := "none"
	if s, ok := f.String("dream_mode"); ok && contains(dreamModes, s) {
		dreamMode = s
	}
	dreamSource := "conversation"
	if s, ok := f.String("dream_source"); ok && contains(dreamSources, s) {
		dreamSource = s
	}

	// The actor and its configuration are written on after the client hangs
	// up, as in Node, and so are the welcome templates after the commit.
	ctx, cancel := detached(r)
	defer cancel()
	tx, err := rt.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// The new actor inherits the creator's realms and, unless it is virtual,
	// the creator's email; an explicit email wins.
	var (
		creatorRealms []string
		creatorEmail  *string
		creatorFound  = true
	)
	err = tx.QueryRow(ctx, "SELECT realms, email FROM actors WHERE id = $1", User(r).ID).Scan(&creatorRealms, &creatorEmail)
	if errors.Is(err, pgx.ErrNoRows) {
		creatorFound = false
	} else if err != nil {
		return err
	}
	realms := []string{defaultRealm}
	if creatorFound && len(creatorRealms) > 0 {
		realms = creatorRealms
	}
	var actorEmail *string
	if emailIsString && jsutil.Trim(email) != "" {
		trimmed := jsutil.Trim(email)
		actorEmail = &trimmed
	} else if !virtualTrue && creatorFound && creatorEmail != nil && *creatorEmail != "" {
		actorEmail = creatorEmail
	}
	status := "active"
	if virtualTrue {
		status = "available"
	}
	// A note saved into the namespace since the availability check would
	// belong to this actor: lock documents writes and check again (LLM-931,
	// as registration does).
	claim, err := actors.ClaimNamespace(ctx, tx, actorName)
	if err != nil {
		return err
	}
	if !claim.Available {
		httpapi.WriteError(w, http.StatusConflict, "ALREADY_EXISTS", claim.Reason)
		return nil
	}
	var actorID int
	err = tx.QueryRow(ctx,
		`INSERT INTO actors (name, token_hash, token_salt, password_hash, password_salt, status, created_by, realms, email)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		actorName, phraseHash, phraseSalt, passwordHash, passwordSalt, status, User(r).ID, realms, actorEmail).Scan(&actorID)
	if err != nil {
		return err
	}

	daily, err := parseCostBudget(f.Raw("cost_budget_daily"), "cost_budget_daily")
	if err != nil {
		return err
	}
	monthly, err := parseCostBudget(f.Raw("cost_budget_monthly"), "cost_budget_monthly")
	if err != nil {
		return err
	}
	// The `value || null` text columns. Node bound them as they were, so
	// the refusal of another JSON type comes here, after every check Node
	// made, and rolls back the actor as a budget refusal does.
	var text [3]any
	for i, field := range []string{"provider", "model", "personality"} {
		s, err := f.OptionalString(field)
		if err != nil {
			return err
		}
		if s != "" {
			text[i] = s
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO agent_configuration (actor_id, provider, model, virtual, personality, cost_budget_daily, cost_budget_monthly, cache_prompts, learning_enabled, max_tokens, temperature, configuration, dream_mode, dream_source)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
		actorID, text[0], text[1], virtualTrue, text[2], daily, monthly,
		string(f.Raw("cache_prompts")) == "true", string(f.Raw("learning_enabled")) != "false",
		maxTokens, temperature, configuration, dreamMode, dreamSource); err != nil {
		return err
	}
	// Every MCP permission (full tool access), for an agent that is not
	// virtual. The creator sees the new actor through created_by, with no
	// visibility row.
	if !virtualTruthy {
		if _, err := tx.Exec(ctx, `INSERT INTO agent_permissions (actor_id, permission_id)
                 SELECT $1, id FROM permissions`, actorID); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	rt.Visibility.ClearCache(httpapi.StateOf(r).ActorID)

	// The welcome templates run after the commit: a failure is a warning on
	// the answer, and the actor stays.
	var warning string
	welcomeMailSent := false
	if f.Truthy("welcome_template_id") && !virtualTruthy {
		sent, err := rt.applyWelcomeTemplate(ctx, actorID, actorName, pgbind.Value(f.Raw("welcome_template_id")))
		if err != nil {
			rt.Log.Errorf("Post-commit welcome template error: %s", httpapi.ErrorMessage(err))
			warning = "Actor created but welcome template failed: " + httpapi.ErrorMessage(err)
		}
		welcomeMailSent = sent
	}
	welcomeNoteSaved := false
	if f.Truthy("welcome_note_template_id") && !virtualTruthy {
		saved, err := rt.applyWelcomeNoteTemplate(ctx, actorID, actorName, pgbind.Value(f.Raw("welcome_note_template_id")))
		if err != nil {
			rt.Log.Errorf("Post-commit welcome-note template error: %s", httpapi.ErrorMessage(err))
			if warning == "" {
				warning = "Actor created but welcome note failed: " + httpapi.ErrorMessage(err)
			}
		}
		welcomeNoteSaved = saved
	}

	rt.LogAdmin("actor_create", logger.Fields{
		{Key: "name", Value: actorName}, {Key: "virtual", Value: virtualTrue}, {Key: "ui_access", Value: uiAccess},
		{Key: "welcome_mail", Value: welcomeMailSent}, {Key: "welcome_note", Value: welcomeNoteSaved},
		{Key: "user_id", Value: User(r).ID},
	})
	message := "Actor created. Save the passphrase — it will not be shown again."
	if virtualTruthy {
		message = "Virtual agent created."
	}
	return httpapi.WriteJSON(w, http.StatusOK, actorCreated{
		ActorID: actorID, Name: actorName, Passphrase: phrase, Virtual: virtualTrue, UIAccess: uiAccess,
		Status: "active", WelcomeMailSent: welcomeMailSent, WelcomeNoteSaved: welcomeNoteSaved,
		Message: message, Warning: warning,
	})
}

// templateByID is the content of the template with that id and kind; found
// is false when there is none. id is bound as Node bound the body value.
func (rt *Routes) templateByID(ctx context.Context, id any, kind string) (frontmatter.Header, string, bool, error) {
	var content *string
	err := rt.DB.QueryRow(ctx, "SELECT content FROM templates WHERE id = $1 AND kind = $2", id, kind).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	if content == nil {
		return nil, "", false, errors.New("Cannot read properties of null (reading 'match')")
	}
	header, body := frontmatter.Parse(*content)
	return header, body, true, nil
}

// applyWelcomeTemplate copies a welcome template into the new agent's
// startup instructions and sends it as mail from system.
func (rt *Routes) applyWelcomeTemplate(ctx context.Context, actorID int, actorName string, templateID any) (bool, error) {
	header, body, found, err := rt.templateByID(ctx, templateID, "welcome")
	if err != nil || !found {
		return false, err
	}
	mailBody := jsutil.ReplaceAll(body, "{agent}", actorName)
	if _, err := rt.DB.Exec(ctx, "UPDATE agent_configuration SET startup_instructions = $1 WHERE actor_id = $2", mailBody, actorID); err != nil {
		return false, err
	}
	subject := jsutil.ReplaceAll(header.Get("subject", "Welcome, {agent}"), "{agent}", actorName)
	if _, err := rt.Mail.Send(ctx, actorName, "system", subject, mailBody, ""); err != nil {
		return false, err
	}
	return true, nil
}

// applyWelcomeNoteTemplate saves a welcome-note template as a note in the
// new agent's namespace.
func (rt *Routes) applyWelcomeNoteTemplate(ctx context.Context, actorID int, actorName string, templateID any) (bool, error) {
	header, body, found, err := rt.templateByID(ctx, templateID, "welcome-note")
	if err != nil || !found {
		return false, err
	}
	// Node passed the new actor's id where saveNote takes a creator name, so
	// the creator is looked up by that number as a name.
	_, err = rt.Notes.Save(ctx, notes.SaveInput{
		Namespace: actorName,
		Title:     jsutil.ReplaceAll(header.Get("title", "Getting Started"), "{agent}", actorName),
		Content:   jsutil.ReplaceAll(body, "{agent}", actorName),
		Slug:      header.Get("slug", "instructions/getting-started"),
		CreatedBy: strconv.Itoa(actorID),
	})
	if err != nil {
		return false, err
	}
	return true, nil
}
