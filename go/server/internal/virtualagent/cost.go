package virtualagent

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
)

// parseFloatJS is parseFloat(s), NaN where JavaScript gives NaN.
func parseFloatJS(s string) float64 {
	f, ok := jsutil.ParseFloat(s)
	if !ok {
		return math.NaN()
	}
	return f
}

// CostLimits are an agent's daily and 30-day cost limits in dollars; nil
// is unlimited. A limit may be NaN (a setting parseFloat cannot read), which
// never trips.
type CostLimits struct {
	Daily   *float64
	Monthly *float64
}

// ResolveEffectiveLimits is resolveEffectiveLimits: the agent's own budget
// columns, else the config defaults.
func (s *Service) ResolveEffectiveLimits(a *Agent) (CostLimits, error) {
	defaultDaily, err := s.Config.Get("virtual_agent_default_daily_budget")
	if err != nil {
		return CostLimits{}, err
	}
	defaultMonthly, err := s.Config.Get("virtual_agent_default_monthly_budget")
	if err != nil {
		return CostLimits{}, err
	}
	pick := func(column *string, fallback string) *float64 {
		v := fallback
		if column != nil {
			v = *column
		}
		f := parseFloatJS(v)
		return &f
	}
	return CostLimits{
		Daily:   pick(a.CostBudgetDaily, defaultDaily),
		Monthly: pick(a.CostBudgetMonthly, defaultMonthly),
	}, nil
}

// isOverCostLimit checks the agent's spend today (UTC) and over the last 30
// days against its limits. reason is empty when it is under them.
func (s *Service) isOverCostLimit(ctx context.Context, a *Agent) (reason string, err error) {
	limits, err := s.ResolveEffectiveLimits(a)
	if err != nil {
		return "", err
	}
	if limits.Daily == nil && limits.Monthly == nil {
		return "", nil
	}
	var today, monthly string
	err = s.DB.QueryRow(ctx,
		`SELECT
            COALESCE(SUM(CASE WHEN created_at >= DATE_TRUNC('day', NOW() AT TIME ZONE 'UTC') THEN cost ELSE 0 END), 0)::text AS cost_today,
            COALESCE(SUM(cost), 0)::text AS cost_monthly
         FROM virtual_agent_usage
         WHERE actor_id = $1 AND created_at >= NOW() - INTERVAL '30 days'`, a.ActorID).Scan(&today, &monthly)
	if err != nil {
		return "", err
	}
	costToday, costMonthly := parseFloatJS(today), parseFloatJS(monthly)
	if limits.Daily != nil && costToday >= *limits.Daily {
		s.logVA("over-cost-limit", logger.Fields{
			{Key: "agent", Value: a.Name}, {Key: "costToday", Value: jsNumber(costToday)},
			{Key: "dailyLimit", Value: jsNumber(*limits.Daily)}, {Key: "type", Value: "daily"},
		})
		return "Daily cost limit exceeded ($" + jsutil.ToFixed(costToday, 4) + " / $" + jsutil.ToFixed(*limits.Daily, 2) + ")", nil
	}
	if limits.Monthly != nil && costMonthly >= *limits.Monthly {
		s.logVA("over-cost-limit", logger.Fields{
			{Key: "agent", Value: a.Name}, {Key: "costMonthly", Value: jsNumber(costMonthly)},
			{Key: "monthlyLimit", Value: jsNumber(*limits.Monthly)}, {Key: "type", Value: "monthly"},
		})
		return "Monthly cost limit exceeded ($" + jsutil.ToFixed(costMonthly, 4) + " / $" + jsutil.ToFixed(*limits.Monthly, 2) + ")", nil
	}
	return "", nil
}

// orZero is `v || 0` for a provider usage value.
func orZero(v any) any {
	if jsutil.Truthy(v) {
		return v
	}
	return 0
}

// bindJS is a JavaScript value as node-postgres bound it (prepareValue):
// null for null and undefined, a number as its JS text, true / false as
// text, an array as a Postgres array literal, an object as JSON.stringify.
// The parameter is text, which Postgres reads for the column's type, so a
// value it cannot read is refused with its own message, as for Node.
func bindJS(v any) any {
	text, ok := prepareValue(v)
	if !ok {
		return nil
	}
	if _, isNumber := jsutil.AsFloat(v); isNumber {
		return jsutil.Number(text)
	}
	return text
}

// prepareValue is node-postgres's prepareValue for a jsutil value; ok is
// false for its null.
func prepareValue(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case []any:
		return arrayString(x), true
	case *jsutil.Object:
		return jsutil.Marshal(x), true
	}
	if v == jsutil.Undefined {
		return "", false
	}
	if f, ok := jsutil.AsFloat(v); ok {
		return jsutil.NumberString(f), true
	}
	return jsutil.Marshal(v), true
}

// arrayString is node-postgres's array literal: null and undefined elements
// are NULL, a nested array nests, anything else is its prepared text in
// double quotes with backslash and quote escaped.
func arrayString(a []any) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, item := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		if item == nil || item == jsutil.Undefined {
			b.WriteString("NULL")
			continue
		}
		if nested, ok := item.([]any); ok {
			b.WriteString(arrayString(nested))
			continue
		}
		text, _ := prepareValue(item)
		text = strings.ReplaceAll(text, `\`, "\\\\")
		text = strings.ReplaceAll(text, `"`, `\"`)
		b.WriteString(`"` + text + `"`)
	}
	b.WriteByte('}')
	return b.String()
}

// logJS makes a JavaScript value encodable in a log line, as
// JSON.stringify wrote it.
func logJS(v any) any {
	switch x := v.(type) {
	case *jsutil.Object, []any:
		return rawJSON(jsutil.Marshal(x))
	case float64:
		return jsNumber(x)
	}
	if v == jsutil.Undefined {
		return nil
	}
	return v
}

type rawJSON string

func (r rawJSON) MarshalJSON() ([]byte, error) {
	return []byte(r), nil
}

// recordUsage is recordUsage: one virtual_agent_usage row with the call's
// cost, returning its id. status is success unless given.
func (s *Service) recordUsage(ctx context.Context, agentName, provider, model string, usage providers.Usage, usageContext, status, errorMessage string) (int, error) {
	cost, err := providers.CalculateCost(provider, model, usage)
	if err != nil {
		return 0, err
	}
	actor, err := s.Actors.RequireByName(ctx, agentName)
	if err != nil {
		return 0, err
	}
	if status == "" {
		status = "success"
	}
	var id int
	err = s.DB.QueryRow(ctx,
		`INSERT INTO virtual_agent_usage (actor_id, provider, model, input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost, context, status, error_message)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
         RETURNING id`,
		actor.ID, provider, model,
		bindJS(orZero(usage.InputTokens)), bindJS(orZero(usage.OutputTokens)),
		bindJS(orZero(usage.CacheCreationInputTokens)), bindJS(orZero(usage.CacheReadInputTokens)),
		jsutil.Num(cost), nullable(usageContext), status, nullable(errorMessage)).Scan(&id)
	if err != nil {
		return 0, err
	}
	fields := logger.Fields{
		{Key: "agent", Value: agentName}, {Key: "provider", Value: provider}, {Key: "model", Value: model},
		{Key: "cost", Value: jsutil.ToFixed(cost, 6)},
	}
	if usageContext != "" {
		fields = append(fields, logger.Field{Key: "context", Value: usageContext})
	}
	fields = append(fields,
		logger.Field{Key: "input", Value: logJS(orZero(usage.InputTokens))},
		logger.Field{Key: "output", Value: logJS(orZero(usage.OutputTokens))},
		logger.Field{Key: "status", Value: status},
	)
	s.logVA("usage-recorded", fields)
	return id, nil
}

// nullable is `s || null`.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
