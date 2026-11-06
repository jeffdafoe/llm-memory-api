package agent

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/virtualagent"
)

// VirtualAgents is the part of the virtual-agent service that /agent/tick
// and /agent/rate-limit call (*virtualagent.Service).
type VirtualAgents interface {
	Invoke(ctx context.Context, agentName string, opts virtualagent.InvokeOptions) (*virtualagent.InvokeResult, error)
	EffectiveRateLimit(ctx context.Context, agentName string) (virtualagent.RateLimit, error)
}

// Bounds on one /agent/rate-limit batch: each slug is an override lookup.
const (
	rateLimitBatchMax    = 200
	rateLimitSlugMaxLen  = 200
	rateLimitSlugsError  = "agents must be a non-empty array of agent slugs"
	rateLimitBatchError  = "agents may contain at most 200 slugs"
	rateLimitSlugError   = "each agent must be a non-empty string of at most 200 characters"
	tickNeedsInputError  = "Required: exactly one of perception (string) or messages (array)"
	tickBothInputsError  = "Provide either perception or messages, not both"
	tickToolsError       = "tools must be an array of tool definitions"
	tickSystemError      = "system must be a string"
	tickFallbackMessage  = "tick failed"
	tickRateLimitedMatch = "rate-limited"
	tickCostLimitMatch   = "over cost limit"
)

// rateLimit is POST /agent/rate-limit: the effective rate limit (config
// defaults with each agent's overrides) for a batch of virtual-agent slugs,
// so the salem engine can pace its ticks under the limit. Window and
// cooldown are in milliseconds, the unit the limiter enforces.
func (rt *Routes) rateLimit(w http.ResponseWriter, r *http.Request) error {
	if _, ok := requireAgent(w, r); !ok {
		return nil
	}
	raw := httpapi.StateOf(r).Body
	if raw == nil {
		return httpapi.NoBody("agents")
	}
	body, err := jsutil.Parse(raw)
	if err != nil {
		return err
	}
	agents, isArray := member(body, "agents").([]any)
	if !isArray || len(agents) == 0 {
		return httpapi.BadRequest(w, rateLimitSlugsError)
	}
	if len(agents) > rateLimitBatchMax {
		return httpapi.BadRequest(w, rateLimitBatchError)
	}
	// Node checked every entry before any lookup, then deduplicated with a
	// Set, which keeps first-seen order.
	unique := make([]string, 0, len(agents))
	seen := make(map[string]bool, len(agents))
	for _, v := range agents {
		slug, ok := v.(string)
		if !ok || slug == "" || jsutil.UTF16Len(slug) > rateLimitSlugMaxLen {
			return httpapi.BadRequest(w, rateLimitSlugError)
		}
		if !seen[slug] {
			seen[slug] = true
			unique = append(unique, slug)
		}
	}

	limits := jsutil.NewObject()
	for _, slug := range unique {
		eff, err := rt.Agents.EffectiveRateLimit(r.Context(), slug)
		if err != nil {
			return err
		}
		// limits["__proto__"] = {...} set the object's prototype in Node,
		// so JSON.stringify left that slug out. The lookup still ran.
		if slug == "__proto__" {
			continue
		}
		// NaN (a setting parseInt cannot read) is written as null, as
		// JSON.stringify writes it.
		entry := jsutil.NewObject()
		entry.Set("limit", eff.Limit)
		entry.Set("window_ms", eff.WindowMs)
		entry.Set("cooldown_ms", eff.CooldownMs)
		limits.Set(slug, entry)
	}
	resp := jsutil.NewObject()
	resp.Set("limits", limits)
	return writeObject(w, resp)
}

// tick is POST /agent/tick: one call to the authenticated virtual agent's
// model with a perception (one user message) or a whole conversation, and
// optional tools and system prompt. The salem engine runs the tool loop on
// its side and calls again with the updated messages.
func (rt *Routes) tick(w http.ResponseWriter, r *http.Request) error {
	agent, ok := requireAgent(w, r)
	if !ok {
		return nil
	}
	raw := httpapi.StateOf(r).Body
	if raw == nil {
		return httpapi.NoBody("perception")
	}
	body, err := jsutil.Parse(raw)
	if err != nil {
		return err
	}
	perception, _ := member(body, "perception").(string)
	messages := member(body, "messages")
	tools := member(body, "tools")
	system := member(body, "system")

	hasPerception := perception != ""
	hasMessages := nonEmptyArray(messages)
	if !hasPerception && !hasMessages {
		return httpapi.BadRequest(w, tickNeedsInputError)
	}
	if hasPerception && hasMessages {
		return httpapi.BadRequest(w, tickBothInputsError)
	}
	if tools != jsutil.Undefined {
		if _, ok := tools.([]any); !ok {
			return httpapi.BadRequest(w, tickToolsError)
		}
	}
	systemText, systemIsString := system.(string)
	if system != jsutil.Undefined && !systemIsString {
		return httpapi.BadRequest(w, tickSystemError)
	}

	opts := virtualagent.InvokeOptions{Context: "tick"}
	if hasPerception {
		opts.UserMessage = perception
	}
	if hasMessages {
		opts.Messages = messages
	}
	if nonEmptyArray(tools) {
		opts.Tools = tools
	}
	if systemIsString {
		prompt := providers.TextPrompt(systemText)
		opts.SystemPrompt = &prompt
	}

	// Node's call ran on after the client hung up, so the usage, call log
	// and rate-limit count are written either way. Detached, the reads
	// before the provider call would have no bound, so the whole call gets
	// one past the provider's own; Invoke's writes after the call drop it
	// and keep their own.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), rt.tickTimeout())
	defer cancel()
	result, err := rt.Agents.Invoke(ctx, agent, opts)
	if err != nil {
		// Node matched the message text, not the error type, so any error
		// whose text has these words is answered the same way. The
		// virtual-agent refusals (RateLimitedError, OverCostLimitError)
		// carry them.
		message := err.Error()
		if message == "" {
			message = tickFallbackMessage
		}
		if strings.Contains(message, tickRateLimitedMatch) {
			httpapi.WriteError(w, http.StatusTooManyRequests, "RATE_LIMITED", message)
			return nil
		}
		if strings.Contains(message, tickCostLimitMatch) {
			httpapi.WriteError(w, http.StatusPaymentRequired, "COST_LIMIT", message)
			return nil
		}
		return err
	}

	encoded, err := result.MarshalJSON()
	if err != nil {
		return err
	}
	parsed, err := jsutil.Parse(encoded)
	if err != nil {
		return err
	}
	resp := jsutil.NewObject()
	resp.Set("agent", agent)
	for _, key := range []string{"text", "tool_calls", "usage", "cost"} {
		resp.Set(key, member(parsed, key))
	}
	return writeObject(w, resp)
}

// tickMargin is the time a tick may take beyond the provider call's own
// timeout, for the reads before it.
const tickMargin = 60 * time.Second

// tickTimeout is the provider timeout as the providers read it
// (virtual_agent_request_timeout: parseInt, else 120; setTimeout's range,
// else 1 ms) plus tickMargin. A missing key is left to Invoke, which fails
// on it.
func (rt *Routes) tickTimeout() time.Duration {
	seconds := 120.0
	if value, err := rt.Config.Get("virtual_agent_request_timeout"); err == nil {
		if n, ok := jsutil.ParseInt(value); ok && n != 0 {
			seconds = n
		}
	}
	ms := seconds * 1000
	if !(ms >= 1 && ms <= 2147483647) {
		ms = 1
	}
	return time.Duration(ms)*time.Millisecond + tickMargin
}

func nonEmptyArray(v any) bool {
	a, ok := v.([]any)
	return ok && len(a) > 0
}
