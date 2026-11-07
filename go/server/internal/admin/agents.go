package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/diceware"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgbind"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgjson"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sanitize"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/virtualagent"
)

// defaultStorageQuota is the per-agent quota when default_storage_quota does
// not parse (50 MB).
const defaultStorageQuota = 52428800

// The dream modes and sources an agent may have.
var (
	dreamModes   = []string{"none", "companion", "technical", "sim", "sim-shared"}
	dreamSources = []string{"conversation", "notes"}
)

// agentField is sanitize.agentName(req.body.agent). given is Node's `agent`
// truthiness: a sanitized string that is not empty, or a truthy value of
// another type, which agentName returned as is. That value is refused with
// err, after the route's required-field check.
func agentField(f httpapi.Fields) (agent string, given bool, err error) {
	if s, ok := f.String("agent"); ok {
		agent = sanitize.AgentName(s)
		return agent, agent != "", nil
	}
	if f.Truthy("agent") {
		return "", true, httpapi.NotString("agent")
	}
	return "", false, nil
}

// notFound is the 404 Node wrote directly.
func notFound(w http.ResponseWriter, message string) error {
	httpapi.WriteError(w, http.StatusNotFound, "NOT_FOUND", message)
	return nil
}

// jsNumber is a JavaScript number as JSON.stringify writes it: NaN and the
// infinities are null.
func jsNumber(x float64) json.RawMessage {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return json.RawMessage("null")
	}
	return json.RawMessage(jsutil.NumberString(x))
}

// jsString is s as a JSON string.
func jsString(s string) json.RawMessage {
	b, _ := jsonutil.Marshal(s)
	return b
}

// parseFloatValue is parseFloat(value) for a body field: the value's
// String(), read as a number prefix; NaN when there is none.
func parseFloatValue(raw json.RawMessage) (float64, error) {
	v, err := jsutil.Parse(raw)
	if err != nil {
		return 0, err
	}
	s, err := jsutil.ValueString(v)
	if err != nil {
		return 0, err
	}
	f, ok := jsutil.ParseFloat(s)
	if !ok {
		return math.NaN(), nil
	}
	return f, nil
}

// configurationValue is the parsed `configuration` text column, {} when it
// is empty or does not parse.
func configurationValue(row pgjson.Row) any {
	var conf any = jsutil.NewObject()
	if text, ok := row.String("configuration"); ok && text != "" {
		if parsed, err := jsutil.Parse([]byte(text)); err == nil {
			conf = parsed
		}
	}
	return conf
}

// pricingInfo sets row.pricing_info to the provider's pricing text when the
// row has a provider and a model.
func (rt *Routes) pricingInfo(row *pgjson.Row) error {
	provider, _ := row.String("provider")
	model, _ := row.String("model")
	if provider == "" || model == "" {
		return nil
	}
	text, err := rt.Providers.FormatPricing(provider, model, configurationValue(*row))
	if err != nil {
		return err
	}
	row.Set("pricing_info", jsString(text))
	return nil
}

const agentsListSQL = `SELECT s.agent, s.actor_id, s.status, s.last_seen, s.passphrase_rotated_at, s.registered_at, s.provider, s.model, s.virtual, s.personality, s.active_since,
                s.cost_budget_daily, s.cost_budget_monthly, s.cache_prompts, s.learning_enabled, s.max_tokens, s.temperature, s.dream_mode, ac.dream_source, s.storage_quota, ac.configuration,
                ac.api_key IS NOT NULL AS has_api_key,
                a.realms,
                COALESCE(vis.summary, 'self only') AS visibility_summary,
                va.summary AS va_access_summary
         FROM agent_status s
         LEFT JOIN agent_configuration ac ON ac.actor_id = s.actor_id
         LEFT JOIN actors a ON a.id = s.actor_id
         LEFT JOIN LATERAL (
             SELECT CASE
                 WHEN EXISTS (SELECT 1 FROM actor_visibility_configuration WHERE actor_id = s.actor_id AND target_actor_id IS NULL)
                     THEN 'all agents'
                 WHEN (SELECT COUNT(*) FROM actor_visibility_configuration WHERE actor_id = s.actor_id AND target_actor_id IS NOT NULL) = 0
                     THEN 'self only'
                 ELSE (SELECT COUNT(*)::text || ' agent' || CASE WHEN COUNT(*) != 1 THEN 's' ELSE '' END
                        FROM actor_visibility_configuration WHERE actor_id = s.actor_id AND target_actor_id IS NOT NULL)
             END AS summary
         ) vis ON true
         LEFT JOIN LATERAL (
             SELECT CASE
                 WHEN NOT s.virtual THEN NULL
                 WHEN EXISTS (SELECT 1 FROM virtual_agent_access WHERE virtual_agent_id = s.actor_id AND grantee_actor_id IS NULL)
                     THEN 'public'
                 WHEN (SELECT COUNT(*) FROM virtual_agent_access WHERE virtual_agent_id = s.actor_id AND grantee_actor_id IS NOT NULL) = 0
                     THEN 'creator only'
                 ELSE (SELECT COUNT(*)::text || ' agent' || CASE WHEN COUNT(*) != 1 THEN 's' ELSE '' END
                        FROM virtual_agent_access WHERE virtual_agent_id = s.actor_id AND grantee_actor_id IS NOT NULL)
             END AS summary
         ) va ON true`

// agentsList is POST /admin/agents: the agents the caller may see, with
// visibility and access summaries, pricing text and the global defaults.
func (rt *Routes) agentsList(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	visible, err := rt.Visibility.VisibleActorIDs(ctx, httpapi.StateOf(r).ActorID)
	if err != nil {
		return err
	}
	sql := agentsListSQL
	var args []any
	if !visible.All {
		sql += " WHERE s.actor_id = ANY($1)"
		args = append(args, visible.IDs)
	}
	sql += ` ORDER BY CASE s.status WHEN 'online' THEN 0 WHEN 'available' THEN 1 WHEN 'offline' THEN 2 ELSE 3 END, s.last_seen DESC NULLS LAST`
	rows, err := pgjson.Query(ctx, rt.DB, sql, args...)
	if err != nil {
		return err
	}

	// OpenRouter's pricing text reads the catalog cache, so fill it first.
	for _, row := range rows {
		if p, _ := row.String("provider"); p == "openrouter" {
			rt.Providers.FetchCatalog(ctx)
			break
		}
	}
	for i := range rows {
		if err := rt.pricingInfo(&rows[i]); err != nil {
			return err
		}
		rows[i].Delete("configuration")
	}

	quotaText, err := rt.Config.Get("default_storage_quota")
	if err != nil {
		return err
	}
	quota, ok := jsutil.ParseInt(quotaText)
	if !ok || quota == 0 {
		quota = defaultStorageQuota
	}
	realmsText, err := rt.Config.Get("hosted_realms")
	if err != nil {
		return err
	}
	hosted := []string{}
	for _, realm := range strings.Split(realmsText, ",") {
		if t := jsutil.Trim(realm); t != "" {
			hosted = append(hosted, t)
		}
	}
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Agents              []pgjson.Row    `json:"agents"`
		DefaultStorageQuota json.RawMessage `json:"default_storage_quota"`
		HostedRealms        []string        `json:"hosted_realms"`
	}{rows, jsNumber(quota), hosted})
}

// agentsInstructionsRead is POST /admin/agents/instructions/read: an agent's
// startup instructions.
func (rt *Routes) agentsInstructionsRead(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("agent")
	}
	agent, given, err := agentField(f)
	if !given {
		return httpapi.BadRequest(w, "Required field: agent")
	}
	if err != nil {
		return err
	}
	actor, ok, err := rt.RequireVisibility(w, r, agent)
	if !ok {
		return err
	}
	var instructions *string
	err = rt.DB.QueryRow(r.Context(), "SELECT startup_instructions FROM agent_configuration WHERE actor_id = $1", actor.ID).Scan(&instructions)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound(w, "Agent not found")
	}
	if err != nil {
		return err
	}
	text := ""
	if instructions != nil {
		text = *instructions
	}
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Agent        string `json:"agent"`
		Instructions string `json:"instructions"`
	}{agent, text})
}

// agentsInstructionsSave is POST /admin/agents/instructions/save: replaces
// an agent's startup instructions. Only its creator or a superadmin.
func (rt *Routes) agentsInstructionsSave(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("agent")
	}
	agent, given, agentErr := agentField(f)
	contentRaw, present := f["content"]
	if !given || !present {
		return httpapi.BadRequest(w, "Required fields: agent, content")
	}
	if agentErr != nil {
		return agentErr
	}
	actor, ok, err := rt.RequireVisibility(w, r, agent)
	if !ok {
		return err
	}
	if ok, err := rt.RequireOwnershipID(w, r, actor.ID); !ok {
		return err
	}
	// Node used content only in the update, after both checks. A JSON null
	// is stored as NULL, and then Node failed reading its length.
	var content *string
	if string(contentRaw) != "null" {
		s, err := f.RequireString("content")
		if err != nil {
			return err
		}
		content = &s
	}
	ctx := r.Context()
	var id int
	err = rt.DB.QueryRow(ctx, "UPDATE agent_configuration SET startup_instructions = $1 WHERE actor_id = $2 RETURNING actor_id", content, actor.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound(w, "Agent not found")
	}
	if err != nil {
		return err
	}
	rt.LogAdmin("agent_instructions_save", logger.Fields{{Key: "agent", Value: agent}, {Key: "user_id", Value: User(r).ID}})
	if content == nil {
		return errors.New("Cannot read properties of null (reading 'length')")
	}
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Agent   string `json:"agent"`
		Message string `json:"message"`
		Length  int    `json:"length"`
	}{agent, "Instructions saved", jsutil.UTF16Len(*content)})
}

// agentsExpertiseSave is POST /admin/agents/expertise/save: replaces an
// agent's expertise with the array's non-blank strings, trimmed and lower
// case.
func (rt *Routes) agentsExpertiseSave(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("agent")
	}
	agent, given, agentErr := agentField(f)
	elems, isArray := f.Array("expertise")
	if !given || !isArray {
		return httpapi.BadRequest(w, "Required fields: agent, expertise (array of strings)")
	}
	if agentErr != nil {
		return agentErr
	}
	cleaned := []string{}
	for _, e := range elems {
		var s string
		if len(e) == 0 || e[0] != '"' || json.Unmarshal(e, &s) != nil {
			continue
		}
		if t := jsutil.Trim(s); t != "" {
			cleaned = append(cleaned, jsutil.ToLower(t))
		}
	}
	text, err := jsonutil.Marshal(cleaned)
	if err != nil {
		return err
	}

	actor, ok, err := rt.RequireVisibility(w, r, agent)
	if !ok {
		return err
	}
	if ok, err := rt.RequireOwnershipID(w, r, actor.ID); !ok {
		return err
	}
	ctx := r.Context()
	var id int
	err = rt.DB.QueryRow(ctx, "UPDATE actors SET expertise = $1 WHERE id = $2 RETURNING id", string(text), actor.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound(w, "Agent not found")
	}
	if err != nil {
		return err
	}
	rt.LogAdmin("agent_expertise_save", logger.Fields{{Key: "agent", Value: agent}, {Key: "expertise", Value: cleaned}, {Key: "user_id", Value: User(r).ID}})
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Agent     string   `json:"agent"`
		Expertise []string `json:"expertise"`
		Message   string   `json:"message"`
	}{agent, cleaned, "Expertise saved"})
}

// agentsResetPassphrase is POST /admin/agents/reset-passphrase: a new
// passphrase for the agent, and every API session of it ended. Only its
// creator or a superadmin.
func (rt *Routes) agentsResetPassphrase(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("agent")
	}
	agent, given, err := agentField(f)
	if !given {
		return httpapi.BadRequest(w, "Required field: agent")
	}
	if err != nil {
		return err
	}
	phrase := diceware.Passphrase()
	salt := auth.GenerateSalt()
	hash, err := auth.Hash(phrase, salt)
	if err != nil {
		return err
	}

	actor, ok, err := rt.RequireVisibility(w, r, agent)
	if !ok {
		return err
	}
	if ok, err := rt.RequireOwnershipID(w, r, actor.ID); !ok {
		return err
	}

	// The new hash and the session delete commit together, so a failure
	// cannot leave the old sessions working after the reset.
	ctx := r.Context()
	tx, err := rt.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var id int
	err = tx.QueryRow(ctx,
		"UPDATE actors SET token_hash = $1, token_salt = $2, passphrase_rotated_at = NOW() WHERE id = $3 RETURNING id",
		hash, salt, actor.ID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound(w, "Agent not found")
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM sessions WHERE actor_id = $1 AND kind = $2", actor.ID, auth.SessionKindAPI); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	rt.SessionAuth.ForgetAgent(agent)

	rt.LogAdmin("agent_passphrase_reset", logger.Fields{{Key: "agent", Value: agent}, {Key: "user_id", Value: User(r).ID}})
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Agent      string `json:"agent"`
		Passphrase string `json:"passphrase"`
		Message    string `json:"message"`
	}{agent, phrase, "Passphrase reset. All sessions invalidated."})
}

const agentsReadSQL = `SELECT ac.name AS agent, agc.provider, agc.model, agc.virtual, agc.personality, agc.configuration, ac.expertise,
                agc.cache_prompts, agc.learning_enabled, agc.max_tokens, agc.temperature,
                agc.cost_budget_daily, agc.cost_budget_monthly, agc.storage_quota,
                agc.api_key IS NOT NULL AS has_api_key,
                agc.dream_mode, agc.dream_source, ac.realms
         FROM agent_configuration agc
         JOIN actors ac ON ac.id = agc.actor_id
         WHERE agc.actor_id = $1`

// agentsRead is POST /admin/agents/read: an agent's configuration, its cost
// today (UTC) and over 30 days, its effective limits and pricing text.
func (rt *Routes) agentsRead(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("agent")
	}
	agent, given, err := agentField(f)
	if !given {
		return httpapi.BadRequest(w, "Required field: agent")
	}
	if err != nil {
		return err
	}
	actor, ok, err := rt.RequireVisibility(w, r, agent)
	if !ok {
		return err
	}
	row, found, err := pgjson.QueryOne(ctx, rt.DB, agentsReadSQL, actor.ID)
	if err != nil {
		return err
	}
	if !found {
		return notFound(w, "Agent not found")
	}

	// cost_today is the UTC day, as the cost-limit gate counts it.
	var today, monthly string
	err = rt.DB.QueryRow(ctx,
		`SELECT
            COALESCE(SUM(CASE WHEN created_at >= DATE_TRUNC('day', NOW() AT TIME ZONE 'UTC') THEN cost ELSE 0 END), 0)::text AS cost_today,
            COALESCE(SUM(cost), 0)::text AS cost_monthly
         FROM virtual_agent_usage
         WHERE actor_id = $1 AND created_at >= NOW() - INTERVAL '30 days'`, actor.ID).Scan(&today, &monthly)
	if err != nil {
		return err
	}
	row.Set("cost_today", jsNumber(parseFloatText(today)))
	row.Set("cost_monthly", jsNumber(parseFloatText(monthly)))

	budget := func(column string) *string {
		if s, ok := row.String(column); ok {
			return &s
		}
		return nil
	}
	limits, err := rt.Agents.ResolveEffectiveLimits(&virtualagent.Agent{
		CostBudgetDaily: budget("cost_budget_daily"), CostBudgetMonthly: budget("cost_budget_monthly"),
	})
	if err != nil {
		return err
	}
	row.Set("effective_daily_limit", limitJSON(limits.Daily))
	row.Set("effective_monthly_limit", limitJSON(limits.Monthly))

	model, _ := row.String("model")
	if provider, _ := row.String("provider"); provider == "openrouter" && model != "" {
		rt.Providers.FetchCatalog(ctx)
	}
	if err := rt.pricingInfo(&row); err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, row)
}

// parseFloatText is parseFloat(s).
func parseFloatText(s string) float64 {
	f, ok := jsutil.ParseFloat(s)
	if !ok {
		return math.NaN()
	}
	return f
}

func limitJSON(limit *float64) json.RawMessage {
	if limit == nil {
		return json.RawMessage("null")
	}
	return jsNumber(*limit)
}

// parseCostBudget validates a cost budget: null or "" is unlimited (nil);
// anything else must be a finite Number() from 0 to 99999999.99, else the
// 400 Node threw.
func parseCostBudget(raw json.RawMessage, field string) (any, error) {
	if raw == nil || string(raw) == "null" || string(raw) == `""` {
		return nil, nil
	}
	v, err := jsutil.Parse(raw)
	if err != nil {
		return nil, err
	}
	n, err := jsutil.ValueNumber(v)
	if err != nil {
		return nil, err
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 99999999.99 {
		return nil, apierr.New(http.StatusBadRequest, fmt.Sprintf("Invalid %s: must be a non-negative number up to 99999999.99", field))
	}
	return jsutil.Num(n), nil
}

// configurationText is JSON.stringify(sanitizeAgentConfiguration(value)):
// an object (an array spread into one) without the numeric provider
// settings that are "" (the provider default); any other value as it is.
func configurationText(raw json.RawMessage) (string, error) {
	v, err := jsutil.Parse(raw)
	if err != nil {
		return "", err
	}
	switch t := v.(type) {
	case *jsutil.Object:
		cleaned := t.Clone()
		for _, key := range []string{"temperature", "top_p", "frequency_penalty", "presence_penalty", "max_completion_tokens"} {
			if s, ok := cleaned.Value(key).(string); ok && s == "" {
				cleaned.Delete(key)
			}
		}
		v = cleaned
	case []any:
		// { ...array } has the elements under their indexes.
		spread := jsutil.NewObject()
		for i, e := range t {
			spread.Set(fmt.Sprint(i), e)
		}
		v = spread
	}
	return jsutil.Stringify([]byte(jsutil.Marshal(v)))
}

// agentsUpdate is POST /admin/agents/update: sets the agent configuration
// fields the body has, and the actor's realms when `realms` is an array.
// Only its creator or a superadmin.
func (rt *Routes) agentsUpdate(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("agent")
	}
	agent, given, err := agentField(f)
	if !given {
		return httpapi.BadRequest(w, "Required field: agent")
	}
	if err != nil {
		return err
	}
	actor, ok, err := rt.RequireVisibility(w, r, agent)
	if !ok {
		return err
	}
	if ok, err := rt.RequireOwnershipID(w, r, actor.ID); !ok {
		return err
	}
	var id int
	var virtual bool
	err = rt.DB.QueryRow(r.Context(), "SELECT actor_id, virtual FROM agent_configuration WHERE actor_id = $1", actor.ID).Scan(&id, &virtual)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound(w, "Agent not found")
	}
	if err != nil {
		return err
	}

	var (
		columns []string
		params  []any
	)
	set := func(column string, value any) {
		params = append(params, value)
		columns = append(columns, column)
	}
	present := func(name string) bool {
		_, ok := f[name]
		return ok
	}
	// `value || null` for a text column.
	text := func(name string) error {
		s, err := f.OptionalString(name)
		if err != nil {
			return err
		}
		if s == "" {
			set(name, nil)
		} else {
			set(name, s)
		}
		return nil
	}
	isNullOrEmpty := func(name string) bool {
		raw := string(f.Raw(name))
		return raw == "null" || raw == `""`
	}

	if present("personality") {
		if err := text("personality"); err != nil {
			return err
		}
	}
	if present("api_key") {
		key, err := f.OptionalString("api_key")
		if err != nil {
			return err
		}
		if key == "" {
			set("api_key", nil)
		} else {
			encrypted, err := rt.Providers.EncryptAPIKey(key)
			if err != nil {
				return err
			}
			set("api_key", encrypted)
		}
	}
	if present("configuration") {
		if f.Truthy("configuration") {
			conf, err := configurationText(f.Raw("configuration"))
			if err != nil {
				return err
			}
			set("configuration", conf)
		} else {
			set("configuration", nil)
		}
	}
	if present("provider") {
		if err := text("provider"); err != nil {
			return err
		}
	}
	if present("model") {
		if err := text("model"); err != nil {
			return err
		}
	}
	for _, name := range []string{"cost_budget_daily", "cost_budget_monthly"} {
		if present(name) {
			budget, err := parseCostBudget(f.Raw(name), name)
			if err != nil {
				return err
			}
			set(name, budget)
		}
	}
	if present("cache_prompts") {
		set("cache_prompts", string(f.Raw("cache_prompts")) == "true")
	}
	if present("learning_enabled") {
		set("learning_enabled", string(f.Raw("learning_enabled")) != "false")
	}
	if present("max_tokens") {
		if isNullOrEmpty("max_tokens") {
			set("max_tokens", nil)
		} else {
			n, ok := SafeInt(f.Raw("max_tokens"))
			if !ok || n < 1 {
				return httpapi.BadRequest(w, "max_tokens must be a positive integer")
			}
			set("max_tokens", n)
		}
	}
	if present("temperature") {
		if isNullOrEmpty("temperature") {
			set("temperature", nil)
		} else {
			t, err := parseFloatValue(f.Raw("temperature"))
			if err != nil {
				return err
			}
			set("temperature", jsutil.Num(t))
		}
	}
	if present("storage_quota") {
		if isNullOrEmpty("storage_quota") {
			set("storage_quota", nil)
		} else {
			n, ok := SafeInt(f.Raw("storage_quota"))
			// 0 is valid: no quota.
			if !ok || n < 0 {
				return httpapi.BadRequest(w, "storage_quota must be a non-negative integer")
			}
			set("storage_quota", n)
		}
	}
	if present("dream_mode") {
		mode, isString := f.String("dream_mode")
		if !isString || !contains(dreamModes, mode) {
			return httpapi.BadRequest(w, "dream_mode must be none, companion, technical, sim, or sim-shared")
		}
		set("dream_mode", mode)
	}
	if present("dream_source") {
		source, isString := f.String("dream_source")
		if !isString || !contains(dreamSources, source) {
			return httpapi.BadRequest(w, "dream_source must be conversation or notes")
		}
		set("dream_source", source)
	}

	// realms is on actors, not agent_configuration.
	_, hasRealms := f.Array("realms")
	if len(columns) == 0 && !hasRealms {
		return httpapi.BadRequest(w, "No fields to update")
	}

	ctx := r.Context()
	if len(columns) > 0 {
		assignments := make([]string, len(columns))
		for i, column := range columns {
			assignments[i] = fmt.Sprintf("%s = $%d", column, i+1)
		}
		params = append(params, actor.ID)
		sql := fmt.Sprintf("UPDATE agent_configuration SET %s WHERE actor_id = $%d", strings.Join(assignments, ", "), len(params))
		if _, err := rt.DB.Exec(ctx, sql, params...); err != nil {
			return err
		}
	}
	fields := append([]string{}, columns...)
	if hasRealms {
		if _, err := rt.DB.Exec(ctx, "UPDATE actors SET realms = $1 WHERE id = $2", pgbind.Value(f.Raw("realms")), actor.ID); err != nil {
			return err
		}
		fields = append(fields, "realms")
	}
	rt.LogAdmin("agent_update", logger.Fields{{Key: "agent", Value: agent}, {Key: "fields", Value: fields}, {Key: "user_id", Value: User(r).ID}})
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Agent   string `json:"agent"`
		Updated bool   `json:"updated"`
	}{agent, true})
}

// agentsUsage is POST /admin/agents/usage: an agent's newest usage rows
// (limit 1 to 200, default 50), each with its call detail id.
func (rt *Routes) agentsUsage(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("agent")
	}
	agent, given, err := agentField(f)
	if !given {
		return httpapi.BadRequest(w, "Required field: agent")
	}
	if err != nil {
		return err
	}
	actor, ok, err := rt.RequireVisibility(w, r, agent)
	if !ok {
		return err
	}
	limit, ok := SafeInt(f.Raw("limit"))
	if !ok {
		limit = 50
	}
	limit = min(max(limit, 1), 200)
	rows, err := pgjson.Query(r.Context(), rt.DB,
		`SELECT u.id, u.provider, u.model, u.input_tokens, u.output_tokens, u.cache_write_tokens, u.cache_read_tokens,
                u.cost, u.context, u.status, u.error_message, u.created_at,
                (SELECT c.id FROM virtual_agent_calls c WHERE c.usage_id = u.id LIMIT 1) AS call_id
         FROM virtual_agent_usage u
         WHERE u.actor_id = $1
         ORDER BY u.created_at DESC
         LIMIT $2`, actor.ID, limit)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string][]pgjson.Row{"usage": rows})
}

// agentsCallDetail is POST /admin/agents/call-detail: one recorded provider
// call. Node checks no visibility here; the port keeps that.
func (rt *Routes) agentsCallDetail(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return readError("call_id")
	}
	callID, ok := SafeInt(f.Raw("call_id"))
	if !ok || callID <= 0 {
		return httpapi.BadRequest(w, "Required field: call_id")
	}
	row, found, err := pgjson.QueryOne(r.Context(), rt.DB,
		`SELECT c.id, ac.name AS agent, c.context, c.context_id, c.provider, c.model,
                c.sim_actor_id, c.sim_actor_name,
                c.system_prompt, c.user_message, c.response,
                c.status, c.status_code, c.error_message,
                c.input_tokens, c.output_tokens, c.cache_read_tokens, c.cache_write_tokens,
                c.cost, c.duration_ms, c.created_at
         FROM virtual_agent_calls c
         JOIN actors ac ON ac.id = c.actor_id
         WHERE c.id = $1`, callID)
	if err != nil {
		return err
	}
	if !found {
		return notFound(w, "Call detail not found (may have been purged)")
	}
	return httpapi.WriteJSON(w, http.StatusOK, map[string]pgjson.Row{"call": row})
}
