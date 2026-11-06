package virtualagent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
)

// BuildProviderConf is buildProviderConf: the agent's configuration JSON,
// with the legacy cache_prompts / max_tokens / temperature columns filled in
// where the JSON does not set them. A configuration that does not parse is
// {}. A configuration saved for an older version of the model's settings
// is refused, so the agent's owner re-saves it.
//
// The result is a jsutil value. Node assigned the legacy fields onto
// whatever JSON.parse returned; on a primitive that did nothing, so a
// non-object configuration is returned as parsed.
func BuildProviderConf(a *Agent) (any, error) {
	var conf any = jsutil.NewObject()
	if a.Configuration != nil && *a.Configuration != "" {
		if v, err := jsutil.Parse([]byte(*a.Configuration)); err == nil {
			conf = v
		}
	}
	read := func(key string) (any, error) {
		switch x := conf.(type) {
		case nil:
			return nil, fmt.Errorf("Cannot read properties of null (reading '%s')", key)
		case *jsutil.Object:
			return x.Value(key), nil
		}
		return jsutil.Undefined, nil
	}

	if str(a.Provider) != "" && str(a.Model) != "" {
		current, err := providers.GetModelConfigVersion(*a.Provider, *a.Model)
		if err != nil {
			return nil, err
		}
		if current != 0 {
			stored, err := read("_configVersion")
			if err != nil {
				return nil, err
			}
			if !jsutil.Truthy(stored) {
				stored = nil
			}
			if f, ok := jsutil.AsFloat(stored); stored == nil || !ok || f != float64(current) {
				storedText := "none"
				if stored != nil {
					storedText, err = jsutil.ValueString(stored)
					if err != nil {
						return nil, err
					}
				}
				return nil, errors.New("Stale configuration for agent \"" + a.Name + "\" — " +
					"stored config version " + storedText +
					", current version " + fmt.Sprint(current) +
					" for " + *a.Provider + "/" + *a.Model +
					". Re-save the agent's settings in the admin dashboard to update.")
			}
		}
	}

	o, isObject := conf.(*jsutil.Object)
	if conf == nil {
		_, err := read("cache_prompts")
		return nil, err
	}
	if !isObject {
		return conf, nil
	}
	if o.Value("cache_prompts") == jsutil.Undefined {
		o.Set("cache_prompts", a.CachePrompts)
	}
	if o.Value("max_tokens") == jsutil.Undefined && a.MaxTokens != nil {
		o.Set("max_tokens", float64(*a.MaxTokens))
	}
	if o.Value("temperature") == jsutil.Undefined && a.Temperature != nil {
		o.Set("temperature", jsutil.StringToNumber(*a.Temperature))
	}
	return o, nil
}

// InvokeOptions are invokeAgent's options.
type InvokeOptions struct {
	// SystemPrompt replaces the agent's startup_instructions when set.
	SystemPrompt *providers.SystemPrompt
	// UserMessage is the single user message; required unless Messages is
	// a non-empty array.
	UserMessage string
	// Messages is the whole conversation in OpenAI shape (a jsutil value).
	Messages any
	// Tools are neutral {name, description, parameters} tool definitions (a
	// jsutil value).
	Tools any
	// Context is the usage label (dream, soul, learning, tick, ping). Every
	// Node caller passed one; empty is no label.
	Context       string
	SkipRateLimit bool
	SkipCostLimit bool
	// Retry runs the call under retryWithBackoff (Node's skipRetry: false).
	Retry bool
}

// InvokeResult is invokeAgent's answer.
type InvokeResult struct {
	Text         any
	ToolCalls    []providers.ToolCall
	Usage        providers.Usage
	Cost         float64
	FinishReason string
	Truncated    bool
}

// MarshalJSON writes Node's object: text, tool_calls, usage, cost,
// finish_reason, truncated.
func (r *InvokeResult) MarshalJSON() ([]byte, error) {
	usage, _ := r.Usage.MarshalJSON()
	usageValue, _ := jsutil.Parse(usage)
	calls := make([]any, len(r.ToolCalls))
	for i, tc := range r.ToolCalls {
		b, _ := tc.MarshalJSON()
		calls[i], _ = jsutil.Parse(b)
	}
	o := jsutil.NewObject()
	o.Set("text", r.Text)
	o.Set("tool_calls", calls)
	o.Set("usage", usageValue)
	o.Set("cost", r.Cost)
	o.Set("finish_reason", r.FinishReason)
	o.Set("truncated", r.Truncated)
	return []byte(jsutil.Marshal(o)), nil
}

func nonEmptyArray(v any) bool {
	a, ok := v.([]any)
	return ok && len(a) > 0
}

// Invoke is invokeAgent: one call to a virtual agent's model, with the
// rate and cost limits, the agent marked active while it runs, and the
// usage and call recorded. A failed call is recorded as an error row and
// returned.
func (s *Service) Invoke(ctx context.Context, agentName string, opts InvokeOptions) (*InvokeResult, error) {
	hasMessages := nonEmptyArray(opts.Messages)
	if !hasMessages && opts.UserMessage == "" {
		return nil, errors.New("invokeAgent: either userMessage or messages is required")
	}
	agent, err := s.LoadAgent(ctx, agentName)
	if err != nil {
		return nil, err
	}
	if agent == nil {
		return nil, errors.New("Agent not found: " + agentName)
	}
	if str(agent.APIKey) == "" || str(agent.Provider) == "" || str(agent.Model) == "" {
		return nil, errors.New("Agent " + agentName + " missing provider/model/api_key")
	}
	provider, model := *agent.Provider, *agent.Model

	if !opts.SkipRateLimit {
		limited, err := s.IsRateLimited(ctx, agentName, "")
		if err != nil {
			return nil, err
		}
		if limited {
			return nil, errors.New("Agent " + agentName + " is rate-limited")
		}
	}
	if !opts.SkipCostLimit {
		reason, err := s.isOverCostLimit(ctx, agent)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			return nil, errors.New("Agent " + agentName + " over cost limit: " + reason)
		}
	}

	apiKey, err := s.Providers.DecryptAPIKey(*agent.APIKey)
	if err != nil {
		return nil, err
	}
	conf, err := BuildProviderConf(agent)
	if err != nil {
		return nil, err
	}
	call, err := s.Providers.CreateProvider(provider, model, apiKey, conf)
	if err != nil {
		return nil, err
	}

	system := providers.TextPrompt(str(agent.StartupInstructions))
	if opts.SystemPrompt != nil {
		system = *opts.SystemPrompt
	}

	actor, err := s.Actors.RequireByName(ctx, agentName)
	if err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(ctx, "UPDATE actors SET active_since = NOW(), last_seen = NOW() WHERE id = $1", actor.ID); err != nil {
		return nil, err
	}

	raw := &providers.RawOpts{}
	if nonEmptyArray(opts.Tools) {
		raw.Tools = opts.Tools
	}
	if hasMessages {
		raw.Messages = opts.Messages
	}
	callFn := func(ctx context.Context) (*providers.Result, error) {
		return call(ctx, system, opts.UserMessage, raw)
	}

	// The bookkeeping after the call runs whether or not the caller is
	// still waiting, as Node's did.
	after := context.WithoutCancel(ctx)
	start := s.now()
	var result *providers.Result
	if opts.Retry {
		result, err = retryWithBackoff(ctx, s, agentName, callFn, nil)
	} else {
		result, err = callFn(ctx)
	}
	if err != nil {
		duration := s.now().Sub(start)
		usageID, usageErr := s.recordUsage(after, agentName, provider, model, providers.Usage{}, opts.Context, "error", err.Error())
		s.goLogCall(CallLog{
			ActorID: actor.ID, AgentName: agentName, Context: opts.Context,
			Provider: provider, Model: model,
			SystemPrompt: &system, UserMessage: opts.UserMessage,
			Err: err, Duration: duration, UsageID: usageIDOrNil(usageID, usageErr),
		})
		s.DB.Exec(after, "UPDATE actors SET active_since = NULL, last_seen = NOW() WHERE id = $1", actor.ID)
		return nil, err
	}
	s.DB.Exec(after, "UPDATE actors SET active_since = NULL, last_seen = NOW() WHERE id = $1", actor.ID)

	duration := s.now().Sub(start)
	finish := result.FinishReason
	if finish == "" {
		finish = "other"
	}
	toolCalls := result.ToolCalls
	if toolCalls == nil {
		toolCalls = []providers.ToolCall{}
	}

	if !opts.SkipRateLimit {
		if err := s.RecordCall(agentName, ""); err != nil {
			return nil, err
		}
	}
	cost, err := providers.CalculateCost(provider, model, result.Usage)
	if err != nil {
		return nil, err
	}
	usageID, err := s.recordUsage(after, agentName, provider, model, result.Usage, opts.Context, "", "")
	if err != nil {
		return nil, err
	}
	s.goLogCall(CallLog{
		ActorID: actor.ID, AgentName: agentName, Context: opts.Context,
		Provider: provider, Model: model,
		SystemPrompt: &system, UserMessage: opts.UserMessage,
		Response: result.Text, Usage: &result.Usage, Cost: cost, Duration: duration, UsageID: &usageID,
	})

	var contextField any
	if opts.Context != "" {
		contextField = opts.Context
	}
	s.logVA("invoke", logger.Fields{
		{Key: "agent", Value: agentName}, {Key: "context", Value: contextField},
		{Key: "cost", Value: jsutil.ToFixed(cost, 6)},
		{Key: "input", Value: logJS(orZero(result.Usage.InputTokens))},
		{Key: "output", Value: logJS(orZero(result.Usage.OutputTokens))},
		{Key: "tool_calls", Value: len(toolCalls)}, {Key: "finish_reason", Value: finish},
		{Key: "truncated", Value: result.Truncated},
	})
	return &InvokeResult{
		Text: result.Text, ToolCalls: toolCalls, Usage: result.Usage, Cost: cost,
		FinishReason: finish, Truncated: result.Truncated,
	}, nil
}

func usageIDOrNil(id int, err error) *int {
	if err != nil {
		return nil
	}
	return &id
}

// parseBackoffCadence turns a comma-separated seconds cadence ("60,600")
// into millisecond delays. A setting that yields no usable delay falls back
// to the default cadence.
func parseBackoffCadence(value, fallback string) []float64 {
	if value == "" {
		value = fallback
	}
	var out []float64
	for _, part := range strings.Split(value, ",") {
		n := parseInt10(jsutil.Trim(part)) * 1000
		if !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 {
			out = append(out, n)
		}
	}
	if len(out) > 0 {
		return out
	}
	out = nil
	for _, part := range strings.Split(fallback, ",") {
		out = append(out, parseInt10(jsutil.Trim(part))*1000)
	}
	return out
}

// parseInt10 is parseInt(s, 10): leading whitespace, an optional sign,
// then the longest run of decimal digits ("0x10" is 0); NaN with no digits.
func parseInt10(s string) float64 {
	s = jsutil.TrimStart(s)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return math.NaN()
	}
	n, _ := strconv.ParseFloat(s[:end], 64)
	if neg {
		n = -n
	}
	return n
}

// totalDelayForCadence sums the delays retryCount retries would sleep, the
// last entry repeating when the cadence is shorter.
func totalDelayForCadence(cadence []float64, retryCount int) float64 {
	total := 0.0
	for n := 0; n < retryCount; n++ {
		total += cadence[min(n, len(cadence)-1)]
	}
	return total
}

// timerDelay is how long setTimeout waits for ms: under 1 or over 2^31-1
// (or NaN) is 1 ms.
func timerDelay(ms float64) time.Duration {
	if !(ms >= 1 && ms <= 2147483647) {
		ms = 1
	}
	return time.Duration(ms) * time.Millisecond
}

// RetryInfo is what onFirstFailure gets: how many retries are left and how
// long they will take in all.
type RetryInfo struct {
	RetriesRemaining int
	TotalSeconds     float64
}

// errorStatus is err.status as Node's providers set it: the HTTP status of
// a provider's API error.
func errorStatus(err error) (int, bool) {
	var api *providers.APIError
	if errors.As(err, &api) {
		return api.Status, true
	}
	return 0, false
}

// retryWithBackoff is retryWithBackoff: fn is tried, then retried once per
// entry of virtual_agent_retry_backoff, sleeping that long between tries.
// A 4xx other than 429 sleeps on virtual_agent_retry_backoff_client_error
// instead, since a rejected request (often an OpenRouter routing draw)
// usually succeeds at once. The agent's status follows: degraded while it
// retries, available after a recovery, error when every try failed.
// onFirstFailure runs once, after the first failure.
func retryWithBackoff[T any](ctx context.Context, s *Service, agentName string, fn func(context.Context) (T, error), onFirstFailure func(error, RetryInfo) error) (T, error) {
	var zero T
	main, err := s.Config.Get("virtual_agent_retry_backoff")
	if err != nil {
		return zero, err
	}
	backoffs := parseBackoffCadence(main, "60,600,3600")
	clientValue, err := s.Config.Get("virtual_agent_retry_backoff_client_error")
	if err != nil {
		return zero, err
	}
	clientBackoffs := parseBackoffCadence(clientValue, "5,15,60")

	var lastErr error
	for attempt := 0; attempt <= len(backoffs); attempt++ {
		result, err := fn(ctx)
		if err == nil && attempt > 0 {
			err = s.setAgentStatus(ctx, agentName, "available")
		}
		if err == nil {
			return result, nil
		}
		lastErr = err
		if attempt >= len(backoffs) {
			continue
		}
		if err := s.setAgentStatus(ctx, agentName, "degraded"); err != nil {
			return zero, err
		}
		cadence := backoffs
		if status, ok := errorStatus(err); ok && status >= 400 && status < 500 && status != 429 {
			cadence = clientBackoffs
		}
		delay := cadence[min(attempt, len(cadence)-1)]
		s.logVA("retry-backoff", logger.Fields{
			{Key: "agent", Value: agentName}, {Key: "attempt", Value: attempt + 1},
			{Key: "retries", Value: len(backoffs)}, {Key: "delayMs", Value: jsNumber(delay)},
			{Key: "error", Value: err.Error()},
		})
		if attempt == 0 && onFirstFailure != nil {
			total := math.Ceil(totalDelayForCadence(cadence, len(backoffs)) / 1000)
			if cbErr := onFirstFailure(err, RetryInfo{RetriesRemaining: len(backoffs), TotalSeconds: total}); cbErr != nil {
				s.logVA("first-failure-callback-error", logger.Fields{{Key: "agent", Value: agentName}, {Key: "error", Value: cbErr.Error()}})
			}
		}
		if err := s.setTimeout(ctx, delay); err != nil {
			return zero, err
		}
	}
	if err := s.setAgentStatus(ctx, agentName, "error"); err != nil {
		return zero, err
	}
	return zero, lastErr
}

// StartErrorPing starts the error-recovery ping: every
// virtual_agent_error_ping_interval minutes (parseInt, else 15) each
// virtual agent in error or degraded status gets a minimal call, and one
// that answers is set available. It runs until ctx ends; a second call does
// nothing.
func (s *Service) StartErrorPing(ctx context.Context) error {
	value, err := s.Config.Get("virtual_agent_error_ping_interval")
	if err != nil {
		return err
	}
	minutes := float64(jsutil.IntOr(value, 15))
	interval := timerDelay(minutes * 60 * 1000)
	s.pingOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					// setInterval does not wait for the previous run.
					go s.pingErroredAgents(ctx)
				}
			}
		}()
	})
	return nil
}

func (s *Service) pingErroredAgents(ctx context.Context) {
	rows, err := s.DB.Query(ctx,
		`SELECT ac.name AS agent
             FROM actors ac
             JOIN agent_configuration agc ON agc.actor_id = ac.id
             WHERE agc.virtual = TRUE AND ac.status IN ('error', 'degraded')`)
	if err != nil {
		s.logVA("error-ping-scan-failed", logger.Fields{{Key: "error", Value: err.Error()}})
		return
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			s.logVA("error-ping-scan-failed", logger.Fields{{Key: "error", Value: err.Error()}})
			return
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.logVA("error-ping-scan-failed", logger.Fields{{Key: "error", Value: err.Error()}})
		return
	}
	for _, name := range names {
		s.pingAgent(ctx, name)
	}
}

func (s *Service) pingAgent(ctx context.Context, agentName string) {
	prompt := providers.TextPrompt("Respond with OK.")
	_, err := s.Invoke(ctx, agentName, InvokeOptions{
		SystemPrompt: &prompt, UserMessage: "ping", Context: "ping",
		SkipRateLimit: true, SkipCostLimit: true,
	})
	if err == nil {
		err = s.setAgentStatus(ctx, agentName, "available")
	}
	if err != nil {
		s.logVA("error-ping-still-down", logger.Fields{{Key: "agent", Value: agentName}, {Key: "error", Value: err.Error()}})
		return
	}
	s.logVA("error-ping-recovered", logger.Fields{{Key: "agent", Value: agentName}})
}

// statusCode is err.status || err.statusCode for the call log.
func statusCode(err error) any {
	if status, ok := errorStatus(err); ok && status != 0 {
		return status
	}
	var api *apierr.Error
	if errors.As(err, &api) && api.Status != 0 {
		return api.Status
	}
	return nil
}
