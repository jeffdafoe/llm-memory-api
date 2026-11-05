// Package providers is the LLM provider layer (Node's services/providers):
// one adapter per provider (Anthropic, Google, OpenAI, OpenRouter,
// Perplexity, xAI), the model registry with pricing and capabilities, cost
// calculation, and the encryption of stored API keys.
//
// The request each adapter sends (URL, headers it sets, body) and the result
// it returns are Node's. Caller-supplied JSON (messages, tool definitions,
// the agent's configuration) is kept as jsutil values and read with
// JavaScript's rules, so a value of an unexpected type takes the path it took
// in Node, and request bodies are written as JSON.stringify wrote them.
package providers

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// Config is the config table (config.Store).
type Config interface {
	Get(key string) (string, error)
}

// Logger writes activity lines (logger.Logger).
type Logger interface {
	Log(subsystem, action string, details logger.Fields)
}

// Providers holds what the adapters share: the HTTP client, config, the
// logger and the OpenRouter model catalog.
type Providers struct {
	Config Config
	Log    Logger
	Client *http.Client
	// Now is the clock for the catalog TTL; tests replace it.
	Now func() time.Time

	catalogMu sync.Mutex
	catalog   *Catalog
	fetchedAt time.Time
}

func (p *Providers) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

func (p *Providers) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Providers) logProvider(action string, details logger.Fields) {
	if p.Log != nil {
		p.Log.Log("provider", action, details)
	}
}

// SystemPrompt is a system prompt: plain text, or a static prefix (cached by
// Anthropic when caching is on) and a dynamic suffix.
type SystemPrompt struct {
	text       string
	Static     string
	Dynamic    string
	structured bool
}

// TextPrompt is a plain-text system prompt.
func TextPrompt(s string) SystemPrompt {
	return SystemPrompt{text: s}
}

// StructuredPrompt is a {static, dynamic} system prompt.
func StructuredPrompt(static, dynamic string) SystemPrompt {
	return SystemPrompt{Static: static, Dynamic: dynamic, structured: true}
}

// FlattenPrompt joins a structured prompt's non-empty parts with a blank
// line; a plain prompt is returned as is.
func FlattenPrompt(p SystemPrompt) string {
	if !p.structured {
		return p.text
	}
	switch {
	case p.Static != "" && p.Dynamic != "":
		return p.Static + "\n\n" + p.Dynamic
	case p.Static != "":
		return p.Static
	}
	return p.Dynamic
}

// RawOpts is the per-call options bag as a caller built it, each field a
// JavaScript value (nil or jsutil.Undefined when absent):
//
//	Cache        true asks for Anthropic prompt caching (with cache_prompts).
//	Stop         stop sequences.
//	Tools        tool definitions {name, description, parameters}.
//	Messages     the whole conversation in OpenAI shape; replaces the single
//	             user message.
//	TurnContext  volatile context for this call only; needs Messages.
type RawOpts struct {
	Cache       any
	Stop        any
	Tools       any
	Messages    any
	TurnContext any
}

// Opts is RawOpts narrowed to the documented contract (SanitizeOpts).
type Opts struct {
	Cache       bool
	Stop        []string
	Tools       []*jsutil.Object
	Messages    []*jsutil.Object
	TurnContext string
}

// SanitizeOpts keeps the documented fields with the right shapes: stop
// sequences that are non-empty strings, tools and messages that are objects
// with a string name / role, and the turn context only with messages. It
// returns nil when nothing is left.
func SanitizeOpts(raw *RawOpts) *Opts {
	if raw == nil {
		return nil
	}
	out := &Opts{}
	kept := false
	if raw.Cache == true {
		out.Cache = true
		kept = true
	}
	if a, ok := arr(raw.Stop); ok {
		for _, s := range a {
			if str, ok := s.(string); ok && str != "" {
				out.Stop = append(out.Stop, str)
			}
		}
		if len(out.Stop) > 0 {
			kept = true
		}
	}
	if a, ok := arr(raw.Tools); ok {
		for _, t := range a {
			if o, ok := t.(*object); ok && isString(o.Value("name")) {
				out.Tools = append(out.Tools, o)
			}
		}
		if len(out.Tools) > 0 {
			kept = true
		}
	}
	if a, ok := arr(raw.Messages); ok {
		for _, m := range a {
			if o, ok := m.(*object); ok && isString(o.Value("role")) {
				out.Messages = append(out.Messages, o)
			}
		}
		if len(out.Messages) > 0 {
			kept = true
		}
	}
	if len(out.Messages) > 0 {
		if s, ok := raw.TurnContext.(string); ok && s != "" {
			out.TurnContext = s
		}
	}
	if !kept {
		return nil
	}
	return out
}

// AttachTurnContext merges the turn context into an OpenAI-shape
// conversation, for providers that take it inside messages: appended to a
// latest user turn's text, else as a trailing user turn. It returns a new
// slice and leaves the caller's messages unchanged.
func AttachTurnContext(messages []*jsutil.Object, turnContext string) ([]*jsutil.Object, error) {
	if turnContext == "" {
		return messages, nil
	}
	out := append([]*jsutil.Object(nil), messages...)
	if len(out) > 0 && isStr(out[len(out)-1].Value("role"), "user") {
		last := out[len(out)-1]
		var content any = turnContext
		if c := last.Value("content"); truthy(c) {
			s, err := jsutil.ValueString(c)
			if err != nil {
				return nil, err
			}
			content = s + "\n\n" + turnContext
		}
		merged := last.Clone()
		merged.Set("content", content)
		out[len(out)-1] = merged
	} else {
		out = append(out, obj("role", "user", "content", turnContext))
	}
	return out, nil
}

// ToolCall is one tool call in the neutral shape. The values are the
// provider's JavaScript values; a key whose value is jsutil.Undefined is
// left out, as the provider sent no such field.
type ToolCall struct {
	ID    any
	Name  any
	Input any
}

func (t ToolCall) object() *jsutil.Object {
	return obj("id", t.ID, "name", t.Name, "input", t.Input)
}

func (t ToolCall) MarshalJSON() ([]byte, error) {
	return []byte(jsutil.Marshal(t.object())), nil
}

// Usage is token usage as the adapter reported it. A nil field was not set
// (the key is absent in Node's object). Token counts are the provider's
// values, normally numbers.
type Usage struct {
	InputTokens              any
	OutputTokens             any
	CacheCreationInputTokens any
	CacheReadInputTokens     any
	Cost                     *float64
	ServedBy                 *string
}

func (u Usage) MarshalJSON() ([]byte, error) {
	o := jsutil.NewObject()
	set := func(k string, v any) {
		if v != nil {
			o.Set(k, v)
		}
	}
	set("input_tokens", u.InputTokens)
	set("output_tokens", u.OutputTokens)
	set("cache_creation_input_tokens", u.CacheCreationInputTokens)
	set("cache_read_input_tokens", u.CacheReadInputTokens)
	if u.Cost != nil {
		o.Set("cost", *u.Cost)
	}
	if u.ServedBy != nil {
		o.Set("served_by", *u.ServedBy)
	}
	return []byte(jsutil.Marshal(o)), nil
}

// fields is the usage as log fields, for the `...usage` spread in Node's
// log lines.
func (u Usage) fields() logger.Fields {
	var f logger.Fields
	add := func(k string, v any) {
		if v != nil {
			f = append(f, logger.Field{Key: k, Value: logValue(v)})
		}
	}
	add("input_tokens", u.InputTokens)
	add("output_tokens", u.OutputTokens)
	add("cache_creation_input_tokens", u.CacheCreationInputTokens)
	add("cache_read_input_tokens", u.CacheReadInputTokens)
	return f
}

// logValue makes a jsutil value encodable by the logger.
func logValue(v any) any {
	switch x := v.(type) {
	case *jsutil.Object, []any:
		return rawJSON(jsutil.Marshal(x))
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil
		}
	}
	if v == undefined {
		return nil
	}
	return v
}

type rawJSON string

func (r rawJSON) MarshalJSON() ([]byte, error) {
	return []byte(r), nil
}

// Result is one provider call's answer.
type Result struct {
	// Text is the provider's text, normally a string; Node passed some
	// providers' content field through unchanged.
	Text         any
	ToolCalls    []ToolCall
	Usage        Usage
	FinishReason string
	Truncated    bool
}

// MarshalJSON writes the result as Node's object: text, tool_calls, usage,
// finish_reason, truncated.
func (r *Result) MarshalJSON() ([]byte, error) {
	usage, _ := r.Usage.MarshalJSON()
	calls := make([]any, len(r.ToolCalls))
	for i, tc := range r.ToolCalls {
		calls[i] = tc.object()
	}
	return []byte(jsutil.Marshal(obj("text", r.Text, "tool_calls", calls, "usage", rawValue(usage), "finish_reason", r.FinishReason, "truncated", r.Truncated))), nil
}

// rawValue parses JSON this package wrote.
func rawValue(b []byte) any {
	v, _ := jsutil.Parse(b)
	return v
}

// TextString returns Text when it is a string.
func (r *Result) TextString() (string, bool) {
	s, ok := r.Text.(string)
	return s, ok
}

// APIError is a non-2xx answer from a provider. Status lets the caller's
// retry pick a cadence by error class.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return e.Message
}

// callFunc is one adapter's call.
type callFunc func(ctx context.Context, system SystemPrompt, userMessage string, opts *Opts) (*Result, error)

// adapter is one provider module.
type adapter struct {
	name               string
	label              string
	aliases            []string
	models             Models
	acceptsTurnContext bool
	createCall         func(p *Providers, model, apiKey string, conf any) callFunc
	formatPricing      func(p *Providers, modelID string, conf any) string
}

var adapters = []*adapter{anthropicAdapter, googleAdapter, openaiAdapter, openrouterAdapter, perplexityAdapter, xaiAdapter}

var errPrototype = errors.New("prototype")

// resolveProvider finds a provider by name or alias, ignoring case. Node
// looked the lowercased name up in plain objects, so "constructor" and
// "__proto__" found Object's own members; those are errPrototype here, and
// each caller fails as Node then did.
func resolveProvider(name string) (*adapter, error) {
	lower := jsutil.ToLower(name)
	if lower == "constructor" || lower == "__proto__" {
		return nil, errPrototype
	}
	for _, a := range adapters {
		if a.name == lower {
			return a, nil
		}
	}
	for _, a := range adapters {
		for _, alias := range a.aliases {
			if alias == lower {
				return a, nil
			}
		}
	}
	return nil, nil
}

// lookupModel is mod.models[modelID] for an adapter that resolved; a
// prototype member name finds nothing, as its properties were undefined.
func lookupModel(name, modelID string) (*adapter, *Model, error) {
	a, err := resolveProvider(name)
	if err != nil {
		return nil, nil, readErr(undefined, modelID)
	}
	if a == nil {
		return nil, nil, nil
	}
	return a, a.models.Get(modelID), nil
}

// Call is a provider call bound to one agent's provider, model, key and
// configuration.
type Call func(ctx context.Context, system SystemPrompt, userMessage string, opts *RawOpts) (*Result, error)

// CreateProvider returns the call for a provider and model (Node's
// createProvider). The model's apiId, when the registry has one, is what is
// sent. Each call is bounded by virtual_agent_request_timeout seconds
// (parseInt, else 120); past it the call fails with Node's timeout message.
// configuration is the agent's configuration object (a jsutil value).
func (p *Providers) CreateProvider(provider, model, apiKey string, configuration any) (Call, error) {
	mod, err := resolveProvider(provider)
	if err != nil {
		return nil, readErr(undefined, model)
	}
	if mod == nil {
		return nil, fmt.Errorf("Unsupported provider: %s", provider)
	}
	apiModel := model
	if entry := mod.models.Get(model); entry != nil && entry.APIID != "" {
		apiModel = entry.APIID
	}
	callFn := mod.createCall(p, apiModel, apiKey, configuration)

	return func(ctx context.Context, system SystemPrompt, userMessage string, raw *RawOpts) (*Result, error) {
		value, err := p.Config.Get("virtual_agent_request_timeout")
		if err != nil {
			return nil, err
		}
		seconds, ok := jsutil.ParseInt(value)
		if !ok || seconds == 0 {
			seconds = 120
		}
		opts := SanitizeOpts(raw)
		if opts != nil && opts.TurnContext != "" && !mod.acceptsTurnContext {
			merged, err := AttachTurnContext(opts.Messages, opts.TurnContext)
			if err != nil {
				return nil, err
			}
			copied := *opts
			copied.Messages = merged
			copied.TurnContext = ""
			opts = &copied
		}
		return withTimeout(ctx, seconds, provider, model, func(ctx context.Context) (*Result, error) {
			return callFn(ctx, system, userMessage, opts)
		})
	}, nil
}

// withTimeout runs fn under setTimeout's rules: a delay over 2^31-1 ms or
// under 1 ms is 1 ms.
func withTimeout(ctx context.Context, seconds float64, provider, model string, fn func(context.Context) (*Result, error)) (*Result, error) {
	ms := seconds * 1000
	if !(ms >= 1 && ms <= 2147483647) {
		ms = 1
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
	defer cancel()
	res, err := fn(callCtx)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("Provider request timed out after %ss (%s/%s)", jsutil.NumberString(seconds), provider, model)
	}
	return res, err
}

// GetModelPricing returns a model's pricing, or nil.
func GetModelPricing(provider, modelID string) (*Pricing, error) {
	_, m, err := lookupModel(provider, modelID)
	if err != nil || m == nil {
		return nil, err
	}
	return m.Pricing, nil
}

// CalculateCost is the dollar cost of one call: the provider's own cost
// when it reported a finite non-negative one, else the registry pricing
// (per million tokens; Anthropic cache tiers priced apart; Perplexity's
// per-request fee per thousand requests).
func CalculateCost(provider, modelID string, usage Usage) (float64, error) {
	if usage.Cost != nil {
		c := *usage.Cost
		if !math.IsNaN(c) && !math.IsInf(c, 0) && c >= 0 {
			return c, nil
		}
	}
	pricing, err := GetModelPricing(provider, modelID)
	if err != nil {
		return 0, err
	}
	if pricing == nil {
		return 0, fmt.Errorf("No pricing data for %s/%s — cannot calculate cost", provider, modelID)
	}
	tokens := func(v any) float64 {
		if !truthy(v) {
			return 0
		}
		return toNumber(v)
	}
	inputTokens := tokens(usage.InputTokens)
	outputTokens := tokens(usage.OutputTokens)
	cacheWriteTokens := tokens(usage.CacheCreationInputTokens)
	cacheReadTokens := tokens(usage.CacheReadInputTokens)

	cost := 0.0
	cost += inputTokens * pricing.rate(pricing.Input) / 1_000_000
	cost += outputTokens * pricing.rate(pricing.Output) / 1_000_000
	if pricing.truthy(pricing.CacheWrite) {
		cost += cacheWriteTokens * *pricing.CacheWrite / 1_000_000
	}
	if pricing.truthy(pricing.CacheRead) {
		cost += cacheReadTokens * *pricing.CacheRead / 1_000_000
	}
	if pricing.truthy(pricing.Request) {
		cost += *pricing.Request / 1000
	}
	return cost, nil
}

// FormatPricing is the pricing text for the admin UI: the provider's own
// format when it has one, else every listed rate.
func (p *Providers) FormatPricing(provider, modelID string, conf any) (string, error) {
	mod, err := resolveProvider(provider)
	if err == nil && mod != nil && mod.formatPricing != nil {
		return mod.formatPricing(p, modelID, conf), nil
	}
	pricing, err := GetModelPricing(provider, modelID)
	if err != nil {
		return "", err
	}
	if pricing == nil {
		return "No pricing data", nil
	}
	var parts []string
	add := func(v *float64, suffix string) {
		if v != nil {
			parts = append(parts, "$"+jsutil.NumberString(*v)+suffix)
		}
	}
	add(pricing.Input, " in")
	add(pricing.Output, " out")
	add(pricing.CacheWrite, " cache write")
	add(pricing.CacheRead, " cache read")
	add(pricing.Request, "/1K requests")
	return joinParts(parts, " / ") + " per 1M tokens", nil
}

func joinParts(parts []string, sep string) string {
	out := ""
	for i, s := range parts {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

// GetModelConfigVersion returns a model's configuration version, or 0 when
// the provider or model is unknown (Node's null).
func GetModelConfigVersion(provider, modelID string) (int, error) {
	_, m, err := lookupModel(provider, modelID)
	if err != nil || m == nil {
		return 0, err
	}
	return m.ConfigVersion, nil
}

// GetModelCapabilities returns a model's capabilities, or nil.
func GetModelCapabilities(provider, modelID string) (Capabilities, error) {
	_, m, err := lookupModel(provider, modelID)
	if err != nil || m == nil {
		return nil, err
	}
	return m.Capabilities, nil
}

// GetDefaultConfiguration is the configuration a new agent starts with:
// each capability's default, then _configVersion.
func GetDefaultConfiguration(provider, modelID string) (*jsutil.Object, error) {
	caps, err := GetModelCapabilities(provider, modelID)
	if err != nil {
		return nil, err
	}
	defaults := jsutil.NewObject()
	if caps == nil {
		return defaults, nil
	}
	for _, c := range caps {
		defaults.Set(c.Name, c.Default)
	}
	version, err := GetModelConfigVersion(provider, modelID)
	if err != nil {
		return nil, err
	}
	if version != 0 {
		defaults.Set("_configVersion", float64(version))
	}
	return defaults, nil
}

// Registry is the provider and model registry for the admin UI.
func Registry() []byte {
	type entry struct {
		Name   string `json:"name"`
		Label  string `json:"label"`
		Models Models `json:"models"`
	}
	out := struct {
		Providers []entry `json:"providers"`
	}{}
	for _, a := range adapters {
		out.Providers = append(out.Providers, entry{a.name, a.label, a.models})
	}
	b, _ := jsonutil.Marshal(out)
	return b
}
