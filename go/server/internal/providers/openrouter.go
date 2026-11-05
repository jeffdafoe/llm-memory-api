package providers

import (
	"context"
	"math"
	"net/http"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// OpenRouter fronts hundreds of models through one OpenAI-compatible API.
// It has no static models: pricing comes from its /api/v1/models catalog,
// cached for four hours. Cost prefers the billed figure OpenRouter reports
// on each response (one model id is served by many upstream hosts at
// different prices); the catalog price is the fallback.

const catalogTTL = 4 * time.Hour

// CatalogEntry is one catalog model; rates are per million tokens.
type CatalogEntry struct {
	// ID is the catalog's id value, normally a string.
	ID            any
	Input         float64
	Output        float64
	CacheRead     *float64
	ContextLength any
	Name          any
}

// Catalog is the model catalog in OpenRouter's order. Like the Map Node
// kept, an id seen twice keeps its first place and its last value.
type Catalog struct {
	entries []*CatalogEntry
	index   map[any]int
}

func newCatalog() *Catalog {
	return &Catalog{index: map[any]int{}}
}

func (c *Catalog) set(e *CatalogEntry) {
	if k, ok := sameValueKey(e.ID); ok {
		if i, seen := c.index[k]; seen {
			c.entries[i] = e
			return
		}
		c.index[k] = len(c.entries)
	}
	c.entries = append(c.entries, e)
}

// Entries returns the models in order.
func (c *Catalog) Entries() []*CatalogEntry {
	return c.entries
}

// Get returns the model with this string id, or nil.
func (c *Catalog) Get(id string) *CatalogEntry {
	if i, ok := c.index["s"+id]; ok {
		return c.entries[i]
	}
	return nil
}

func (c *Catalog) Len() int {
	return len(c.entries)
}

// FetchCatalog returns the cached catalog, fetching it when it is missing
// or older than four hours. A failed fetch is logged and returns the stale
// catalog, or an empty one, without caching anything.
func (p *Providers) FetchCatalog(ctx context.Context) *Catalog {
	now := p.now()
	p.catalogMu.Lock()
	if p.catalog != nil && now.Sub(p.fetchedAt) < catalogTTL {
		c := p.catalog
		p.catalogMu.Unlock()
		return c
	}
	p.catalogMu.Unlock()

	fallback := func() *Catalog {
		p.catalogMu.Lock()
		defer p.catalogMu.Unlock()
		if p.catalog != nil {
			return p.catalog
		}
		return newCatalog()
	}
	fail := func(err error) *Catalog {
		p.logProvider("catalog-fetch-error", logger.Fields{{Key: "error", Value: err.Error()}})
		return fallback()
	}

	url, err := endpoint("OPENROUTER_API_BASE", "/api/v1/models")
	if err != nil {
		return fail(err)
	}
	resp, err := p.send(ctx, http.MethodGet, url, []header{{"Accept", "application/json"}}, nil)
	if err != nil {
		return fail(err)
	}
	if !resp.ok() {
		p.logProvider("catalog-fetch-error", logger.Fields{{Key: "status", Value: resp.status}})
		return fallback()
	}
	data, err := resp.json()
	if err != nil {
		return fail(err)
	}
	catalog, err := buildCatalog(data)
	if err != nil {
		return fail(err)
	}
	p.catalogMu.Lock()
	p.catalog = catalog
	p.fetchedAt = now
	p.catalogMu.Unlock()
	p.logProvider("catalog-fetched", logger.Fields{{Key: "modelCount", Value: catalog.Len()}})
	return catalog
}

// buildCatalog reads the /api/v1/models answer. Per-token prices become
// per-million rates.
func buildCatalog(data any) (*Catalog, error) {
	list, err := get(data, "data")
	if err != nil {
		return nil, err
	}
	list = or(list, []any{})
	var items []any
	switch x := list.(type) {
	case []any:
		items = x
	case string:
		// A string iterates its characters, none of which has an id.
		items = nil
	default:
		return nil, notIterable("data.data || []")
	}
	catalog := newCatalog()
	for _, m := range items {
		id, err := get(m, "id")
		if err != nil {
			return nil, err
		}
		pricing, _ := get(m, "pricing")
		if !truthy(id) || !truthy(pricing) {
			continue
		}
		input, err := perMillion(or(member(pricing, "prompt"), "0"))
		if err != nil {
			return nil, err
		}
		output, err := perMillion(or(member(pricing, "completion"), "0"))
		if err != nil {
			return nil, err
		}
		var cacheRead *float64
		if cr := member(pricing, "input_cache_read"); truthy(cr) {
			v, err := perMillion(cr)
			if err != nil {
				return nil, err
			}
			cacheRead = &v
		}
		contextLength, _ := get(m, "context_length")
		name, _ := get(m, "name")
		catalog.set(&CatalogEntry{ID: id, Input: input, Output: output, CacheRead: cacheRead, ContextLength: or(contextLength, nil), Name: or(name, id)})
	}
	return catalog, nil
}

// perMillion is parseFloat(v) * 1_000_000.
func perMillion(v any) (float64, error) {
	s, err := jsutil.ValueString(v)
	if err != nil {
		return 0, err
	}
	f, _ := jsutil.ParseFloat(s)
	return f * 1_000_000, nil
}

// notIterable is V8's TypeError for for...of over a non-iterable.
func notIterable(expr string) error {
	return errorString(expr + " is not iterable")
}

// LookupOpenRouterPricing returns a model's catalog pricing, or nil.
func (p *Providers) LookupOpenRouterPricing(ctx context.Context, modelID string) *Pricing {
	e := p.FetchCatalog(ctx).Get(modelID)
	if e == nil {
		return nil
	}
	return &Pricing{Input: num(e.Input), Output: num(e.Output), CacheRead: e.CacheRead}
}

func openrouterComputeCost(ctx context.Context, p *Providers, model string, promptTokens, cachedTokens, completionTokens any) (*float64, error) {
	pricing := p.LookupOpenRouterPricing(ctx, model)
	if pricing == nil {
		return nil, nil
	}
	n := &numbers{}
	orRate := func(vs ...*float64) float64 {
		for _, v := range vs {
			if pricing.truthy(v) {
				return *v
			}
		}
		return 0
	}
	cached := n.of(cachedTokens)
	uncached := n.of(promptTokens) - cached
	cost := 0.0
	cost += uncached * orRate(pricing.Input) / 1_000_000
	cost += cached * orRate(pricing.CacheRead, pricing.Input) / 1_000_000
	cost += n.of(completionTokens) * orRate(pricing.Output) / 1_000_000
	if n.err != nil {
		return nil, n.err
	}
	return &cost, nil
}

// openrouterCapabilities apply to every OpenRouter model.
var openrouterCapabilities = Capabilities{
	stepCap("temperature", "Temperature", "Controls randomness. Lower values are more focused and deterministic, higher values are more creative.", 0.7, 0, 2.0, 0.1),
	numberCap("max_tokens", "Max Output Tokens", "Maximum number of tokens the model will generate in its response.", 4096, 1, 32768),
	selectCap("thinking_effort", "Thinking Effort", "Controls how much the model reasons before responding. \"off\" disables reasoning entirely. Only hybrid reasoning models honour this; others ignore it. LEAVING THIS UNSET IS NOT THE SAME AS \"off\" — unset sends nothing and the model keeps its own default, which for deepseek-v4-flash and Gemini 2.5 means it reasons. Reasoning tokens are billed as output tokens but are NOT stored in the response, so any setting other than \"off\" pays for text that is never kept (LLM-570 follow-up).", "off", "off", "low", "medium", "high", "max"),
}

// OpenRouterCapabilities are the controls every OpenRouter model gets (a
// copy).
func OpenRouterCapabilities() Capabilities {
	return openrouterCapabilities.clone()
}

// Our thinking_effort levels mapped to OpenRouter's reasoning.effort. "off"
// is "none", which suppresses reasoning (exclude would still bill it).
// Absent sends nothing, leaving the model's default, and an unknown value is
// ignored.
var openrouterReasoningEffort = map[string]string{"off": "none", "low": "low", "medium": "medium", "high": "high", "max": "max"}

var openrouterAdapter = &adapter{
	name:          "openrouter",
	label:         "OpenRouter",
	aliases:       []string{"open-router", "open_router"},
	models:        Models{},
	createCall:    openrouterCreateCall,
	formatPricing: openrouterFormatPricing,
}

func openrouterCreateCall(p *Providers, model, apiKey string, configuration any) callFunc {
	conf := or(configuration, obj())
	return func(ctx context.Context, system SystemPrompt, userMessage string, opts *Opts) (*Result, error) {
		msgs := []any{obj("role", "system", "content", FlattenPrompt(system))}
		if opts != nil && len(opts.Messages) > 0 {
			for _, m := range opts.Messages {
				msgs = append(msgs, m)
			}
		} else {
			msgs = append(msgs, obj("role", "user", "content", userMessage))
		}
		body := obj("model", model, "messages", msgs)

		maxTokens, ok, err := asNumber(member(conf, "max_tokens"))
		if err != nil {
			return nil, err
		}
		if ok {
			body.Set("max_tokens", maxTokens)
		}
		temperature, ok, err := asNumber(member(conf, "temperature"))
		if err != nil {
			return nil, err
		}
		if ok {
			body.Set("temperature", temperature)
		}
		if e, ok := member(conf, "thinking_effort").(string); ok {
			if effort, known := openrouterReasoningEffort[e]; known {
				body.Set("reasoning", obj("effort", effort))
			}
		}
		if opts != nil && len(opts.Stop) > 0 {
			body.Set("stop", stringsToValues(opts.Stop, 4))
		}
		useTools := opts != nil && len(opts.Tools) > 0
		if useTools {
			body.Set("tools", toolsFunctionWrapped(opts.Tools))
		}
		// Provider routing (configuration.provider, OpenRouter's own field)
		// pins the upstream host so prompt-prefix caching stays warm. Only a
		// plain object is sent.
		if routing, ok := member(conf, "provider").(*object); ok {
			body.Set("provider", routing)
		}

		p.logProvider("api-call", logger.Fields{{Key: "provider", Value: "openrouter"}, {Key: "model", Value: model}, {Key: "tools", Value: useTools}})

		url, err := endpoint("OPENROUTER_API_BASE", "/api/v1/chat/completions")
		if err != nil {
			return nil, err
		}
		resp, err := p.postJSON(ctx, url, []header{
			{"Content-Type", "application/json"},
			{"Authorization", "Bearer " + apiKey},
			{"HTTP-Referer", "https://memory.jeffdafoe.com"},
			{"X-Title", "llm-memory"},
		}, body)
		if err != nil {
			return nil, err
		}
		if !resp.ok() {
			text := resp.text()
			p.logProvider("api-error", logger.Fields{{Key: "provider", Value: "openrouter"}, {Key: "model", Value: model}, {Key: "status", Value: resp.status}, {Key: "error", Value: text}})
			return nil, apiError("OpenRouter", resp.status, text)
		}
		data, err := resp.json()
		if err != nil {
			return nil, err
		}
		choice, message, err := firstChoice(data, "OpenRouter API returned no content")
		if err != nil {
			return nil, err
		}

		usageData, _ := get(data, "usage")
		usageMember := func(k string) any {
			if !truthy(usageData) {
				return usageData
			}
			return member(usageData, k)
		}
		promptTokens := or(usageMember("prompt_tokens"), 0.0)
		completionTokens := or(usageMember("completion_tokens"), 0.0)
		n := &numbers{}
		cached := 0.0
		if details := usageMember("prompt_tokens_details"); truthy(details) {
			cached = maxNum(0, minNum(n.of(or(member(details, "cached_tokens"), 0.0)), n.of(promptTokens)))
		}
		uncached := maxNum(0, n.of(promptTokens)-cached)
		if n.err != nil {
			return nil, n.err
		}
		usage := Usage{InputTokens: uncached, OutputTokens: completionTokens, CacheReadInputTokens: cached}

		var cost *float64
		if truthy(usageData) {
			if reported := member(usageData, "cost"); !isNullish(reported) {
				n, err := jsutil.ValueNumber(reported)
				if err != nil {
					return nil, err
				}
				if !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 {
					cost = &n
				}
			}
		}
		costSource := "billed"
		if cost == nil {
			costSource = "catalog-estimate"
			cost, err = openrouterComputeCost(ctx, p, model, promptTokens, cached, completionTokens)
			if err != nil {
				return nil, err
			}
		}
		usage.Cost = cost

		// The upstream host that served the call, for spend queries. Trimmed
		// and capped; blank is left unset so the column stays NULL.
		var servedBy any
		if s, ok := member(data, "provider").(string); ok {
			if t := jsutil.Trim(s); t != "" {
				t = jsutil.TruncateUTF16(t, 100)
				usage.ServedBy = &t
				servedBy = t
			}
		}

		toolCalls, err := chatToolCalls(p, "openrouter", model, "((choice.message && choice.message.tool_calls) || []).filter", message, true, opts)
		if err != nil {
			return nil, err
		}
		finishValue, _ := get(choice, "finish_reason")
		finish := NormalizeOpenAIChatFinish(finishValue)

		// Reasoning is billed in completion_tokens but not kept; its size is
		// logged so a non-off effort is visible.
		reasoningChars := 0
		if r, ok := member(message, "reasoning").(string); ok {
			reasoningChars = jsutil.UTF16Len(r)
		}
		source := "unknown"
		if cost != nil {
			source = costSource
		}
		p.logProvider("api-response", logger.Fields{
			{Key: "provider", Value: "openrouter"}, {Key: "model", Value: model},
			{Key: "input", Value: uncached}, {Key: "cached", Value: cached},
			{Key: "output", Value: logValue(completionTokens)}, {Key: "cost", Value: costLog(cost, 8)},
			{Key: "cost_source", Value: source},
			{Key: "served_by", Value: servedBy},
			{Key: "reasoning_chars", Value: reasoningChars},
			{Key: "tool_calls", Value: len(toolCalls)}, {Key: "finish_reason", Value: finish},
		})

		content, _ := get(message, "content")
		return &Result{Text: or(content, ""), ToolCalls: toolCalls, Usage: usage, FinishReason: finish, Truncated: IsTruncated(finish)}, nil
	}
}

// openrouterFormatPricing uses the catalog already fetched, if any.
func openrouterFormatPricing(p *Providers, modelID string, conf any) string {
	p.catalogMu.Lock()
	catalog := p.catalog
	p.catalogMu.Unlock()
	if catalog != nil {
		if e := catalog.Get(modelID); e != nil {
			rate := func(v float64) string {
				return jsutil.NumberString(jsutil.StringToNumber(jsutil.ToFixed(v, 4)))
			}
			parts := []string{"$" + rate(e.Input) + " in", "$" + rate(e.Output) + " out"}
			if e.CacheRead != nil {
				parts = append(parts, "$"+rate(*e.CacheRead)+" cached")
			}
			return joinParts(parts, " / ") + " per 1M tokens"
		}
	}
	return "Pricing loads with model catalog"
}
