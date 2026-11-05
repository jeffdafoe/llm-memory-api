package providers

import (
	"context"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// Models that reject temperature/top_p/top_k with a 400.
var anthropicSamplingRejected = map[string]bool{
	"claude-opus-4-7": true, "claude-opus-4-8": true, "claude-opus-5": true, "claude-opus-5-5": true,
	"claude-sonnet-5": true, "claude-fable-5-1": true, "claude-haiku-5-5": true,
}

// An omitted `thinking` parameter means adaptive thinking on these, so "off"
// must send {type: "disabled"}. Older models run without thinking when it
// is omitted.
var anthropicThinkingOnByDefault = map[string]bool{"claude-opus-5": true, "claude-sonnet-5": true}

// These cannot turn thinking off ({type: "disabled"} is a 400); effort is
// the only control.
var anthropicThinkingAlwaysOn = map[string]bool{"claude-opus-5-5": true, "claude-fable-5-1": true, "claude-haiku-5-5": true}

const anthropicCacheDescription = "Caches the static system prompt and, on tool-use calls, the conversation history across calls. 5-minute TTL, 25% write premium, 90% read discount."

func anthropicCachePrompts() Capability {
	return boolCap("cache_prompts", "Prompt Caching", anthropicCacheDescription, false)
}

func anthropicMaxTokens() Capability {
	return numberCap("max_tokens", "Max Output Tokens", "Maximum number of tokens the model will generate (thinking + response combined). Thinking counts against this, so leave headroom.", 16384, 1, 128000)
}

func anthropicThinkingEffort(canTurnOff bool, def, note string) Capability {
	levels := []string{"low", "medium", "high", "xhigh", "max"}
	if canTurnOff {
		levels = append([]string{"off"}, levels...)
	}
	return selectCap("thinking_effort", "Thinking Effort", "Controls how much the model thinks before responding. Higher effort produces better results on complex tasks but costs more tokens and time. "+note, def, levels...)
}

func anthropicPricing(input, output, cacheWrite, cacheRead float64) *Pricing {
	return &Pricing{Input: num(input), Output: num(output), CacheWrite: num(cacheWrite), CacheRead: num(cacheRead)}
}

func anthropicTemperature(description string) Capability {
	return stepCap("temperature", "Temperature", description, 1.0, 0, 1.0, 0.1)
}

const anthropicThinkingTemperatureNote = "Controls randomness. Lower values are more focused and deterministic, higher values are more creative. Ignored when thinking is enabled."

var anthropicModels = Models{
	{ID: "claude-fable-5-1", Label: "Fable 5.1", APIID: "claude-fable-5-1", ConfigVersion: 1,
		Pricing: anthropicPricing(10, 50, 12.50, 0.25),
		Capabilities: Capabilities{
			anthropicMaxTokens(),
			anthropicThinkingEffort(false, "medium", "Thinking is always on for Fable 5.1 and cannot be turned off. Requires an Anthropic organization with 30-day data retention; long requests can exceed the request timeout at high effort."),
			anthropicCachePrompts(),
		}},
	{ID: "claude-opus-5-5", Label: "Opus 5.5", APIID: "claude-opus-5-5", ConfigVersion: 1,
		Pricing: anthropicPricing(4, 20, 5, 0.20),
		Capabilities: Capabilities{
			anthropicMaxTokens(),
			anthropicThinkingEffort(false, "medium", "Thinking is always on for Opus 5.5 and cannot be turned off; \"medium\" matches Anthropic's own default for this model."),
			anthropicCachePrompts(),
		}},
	{ID: "claude-opus-5", Label: "Opus 5", APIID: "claude-opus-5", ConfigVersion: 1,
		Pricing: anthropicPricing(5, 25, 6.25, 0.50),
		Capabilities: Capabilities{
			anthropicMaxTokens(),
			anthropicThinkingEffort(true, "low", "Opus 5 thinks by default. \"off\" disables it, but Anthropic recommends low effort over off: with thinking off the model sometimes writes a tool call as text instead of calling it."),
			anthropicCachePrompts(),
		}},
	{ID: "claude-sonnet-5", Label: "Sonnet 5", APIID: "claude-sonnet-5", ConfigVersion: 1,
		Pricing: anthropicPricing(2, 10, 2.50, 0.20),
		Capabilities: Capabilities{
			anthropicMaxTokens(),
			anthropicThinkingEffort(true, "low", "Sonnet 5 thinks by default. \"off\" disables it; the model is then less eager to use tools."),
			anthropicCachePrompts(),
		}},
	// Pricing is the tier for prompts up to 100k tokens; longer prompts bill
	// 5x on every field, so their cost is under-recorded.
	{ID: "claude-haiku-5-5", Label: "Haiku 5.5", APIID: "claude-haiku-5-5", ConfigVersion: 1,
		Pricing: anthropicPricing(0.10, 0.50, 0.125, 0.01),
		Capabilities: Capabilities{
			anthropicMaxTokens(),
			anthropicThinkingEffort(false, "low", "Haiku 5.5 thinks by default; effort is the only control. At \"low\" it can skip thinking on simple requests. Anthropic's own default is \"medium\"."),
			anthropicCachePrompts(),
		}},
	{ID: "claude-opus-4-8", Label: "Opus 4.8", APIID: "claude-opus-4-8", ConfigVersion: 1,
		Pricing: anthropicPricing(5, 25, 6.25, 0.50),
		Capabilities: Capabilities{
			anthropicMaxTokens(),
			anthropicThinkingEffort(true, "off", "\"off\" disables thinking entirely. Opus 4.8 does not accept temperature."),
			anthropicCachePrompts(),
		}},
	{ID: "claude-opus-4-7", Label: "Opus 4.7", APIID: "claude-opus-4-7", ConfigVersion: 2,
		Pricing: anthropicPricing(5, 25, 6.25, 0.50),
		Capabilities: Capabilities{
			numberCap("max_tokens", "Max Output Tokens", "Maximum number of tokens the model will generate (thinking + response combined).", 16384, 1, 128000),
			selectCap("thinking_effort", "Thinking Effort", "Controls how much the model thinks before responding. Higher effort produces better results on complex tasks but costs more tokens. \"off\" disables thinking entirely. Opus 4.7 uses adaptive thinking — extended thinking is not supported on this model.", "off", "off", "low", "medium", "high", "xhigh", "max"),
			anthropicCachePrompts(),
		}},
	{ID: "claude-opus-4-6", Label: "Opus 4.6", APIID: "claude-opus-4-6", ConfigVersion: 2,
		Pricing: anthropicPricing(5, 25, 6.25, 0.50),
		Capabilities: Capabilities{
			anthropicTemperature(anthropicThinkingTemperatureNote),
			numberCap("max_tokens", "Max Output Tokens", "Maximum number of tokens the model will generate (thinking + response combined).", 16384, 1, 128000),
			selectCap("thinking_effort", "Thinking Effort", "Controls how much the model thinks before responding. Higher effort produces better results on complex tasks but costs more tokens. \"off\" disables thinking entirely.", "off", "off", "low", "medium", "high", "max"),
			anthropicCachePrompts(),
		}},
	{ID: "claude-sonnet-4-6", Label: "Sonnet 4.6", APIID: "claude-sonnet-4-6", ConfigVersion: 2,
		Pricing: anthropicPricing(3, 15, 3.75, 0.30),
		Capabilities: Capabilities{
			anthropicTemperature(anthropicThinkingTemperatureNote),
			numberCap("max_tokens", "Max Output Tokens", "Maximum number of tokens the model will generate (thinking + response combined).", 8192, 1, 64000),
			selectCap("thinking_effort", "Thinking Effort", "Controls how much the model thinks before responding. Higher effort produces better results on complex tasks but costs more tokens. \"off\" disables thinking entirely.", "off", "off", "low", "medium", "high", "max"),
			anthropicCachePrompts(),
		}},
	{ID: "claude-haiku-4-5", Label: "Haiku 4.5", APIID: "claude-haiku-4-5-20251001", ConfigVersion: 1,
		Pricing: anthropicPricing(1, 5, 1.25, 0.10),
		Capabilities: Capabilities{
			anthropicTemperature("Controls randomness. Lower values are more focused and deterministic, higher values are more creative."),
			numberCap("max_tokens", "Max Output Tokens", "Maximum number of tokens the model will generate in its response.", 4096, 1, 64000),
			anthropicCachePrompts(),
		}},
}

var anthropicAdapter = &adapter{
	name:   "anthropic",
	label:  "Anthropic",
	models: anthropicModels,
	// The call places the turn context itself, after the history cache
	// breakpoint, so CreateProvider must not merge it.
	acceptsTurnContext: true,
	createCall:         anthropicCreateCall,
}

// translateMessagesToAnthropic turns OpenAI-shape messages into Anthropic's:
// tool calls become tool_use blocks in the assistant message, and a run of
// role "tool" messages becomes one user message of tool_result blocks.
// System messages are dropped (the system prompt is a top-level field).
func translateMessagesToAnthropic(messages []*object) ([]*object, error) {
	var out []*object
	var pending []any
	flush := func() {
		if len(pending) > 0 {
			out = append(out, obj("role", "user", "content", pending))
			pending = nil
		}
	}
	for _, msg := range messages {
		role := msg.Value("role").(string)
		if role == "tool" {
			content := msg.Value("content")
			if !isString(content) {
				content = stringifyValue(content)
			}
			pending = append(pending, obj("type", "tool_result", "tool_use_id", msg.Value("tool_call_id"), "content", content))
			continue
		}
		flush()
		switch role {
		case "user":
			out = append(out, obj("role", "user", "content", msg.Value("content")))
		case "assistant":
			var blocks []any
			if c := msg.Value("content"); truthy(c) {
				blocks = append(blocks, obj("type", "text", "text", c))
			}
			if calls, ok := arr(msg.Value("tool_calls")); ok {
				for _, tc := range calls {
					fn, err := get(tc, "function")
					if err != nil {
						return nil, err
					}
					if !truthy(fn) {
						continue
					}
					name, _ := get(fn, "name")
					if !truthy(name) {
						continue
					}
					var input any = obj()
					args, _ := get(fn, "arguments")
					if s, ok := args.(string); ok {
						if parsed, err := parseJSON(s); err == nil {
							input = parsed
						}
					} else if truthy(args) && typeofObject(args) {
						input = args
					}
					id, _ := get(tc, "id")
					blocks = append(blocks, obj("type", "tool_use", "id", id, "name", name, "input", input))
				}
			}
			var content any = blocks
			if blocks == nil {
				content = []any{}
			}
			if len(blocks) == 1 && isStr(blocks[0].(*object).Value("type"), "text") {
				content = blocks[0].(*object).Value("text")
			}
			out = append(out, obj("role", "assistant", "content", content))
		}
	}
	flush()
	return out, nil
}

// markHistoryCacheBreakpoint puts cache_control on the last block of the
// replayed history, so the next round of a tool loop reads this round's
// prefix from the cache. String content becomes a one-block array to carry
// the marker; Anthropic documents a string as shorthand for that block. The
// caller's message objects are copied, not changed.
func markHistoryCacheBreakpoint(messages []*object) error {
	if len(messages) == 0 {
		return nil
	}
	last := messages[len(messages)-1]
	content := last.Value("content")
	if s, ok := content.(string); ok {
		// Anthropic rejects cache_control on an empty text block.
		if s == "" {
			return nil
		}
		last.Set("content", []any{obj("type", "text", "text", s, "cache_control", obj("type", "ephemeral"))})
		return nil
	}
	blocks, ok := arr(content)
	if !ok || len(blocks) == 0 {
		return nil
	}
	block := blocks[len(blocks)-1]
	typ, err := get(block, "type")
	if err != nil {
		return err
	}
	if isStr(typ, "text") {
		text, _ := get(block, "text")
		if !truthy(text) {
			return nil
		}
	}
	// Setting a property on a primitive does nothing, and an array's extra
	// property is not in its JSON.
	if o, ok := block.(*object); ok {
		marked := o.Clone()
		marked.Set("cache_control", obj("type", "ephemeral"))
		copied := append([]any(nil), blocks...)
		copied[len(copied)-1] = marked
		last.Set("content", copied)
	}
	return nil
}

// appendTurnContext adds the turn context as its own text block after the
// history, so the marked block keeps the bytes the next call replays.
func appendTurnContext(messages []*object, turnContext string) []*object {
	block := obj("type", "text", "text", turnContext)
	if len(messages) > 0 && isStr(messages[len(messages)-1].Value("role"), "user") {
		last := messages[len(messages)-1]
		content := last.Value("content")
		if blocks, ok := arr(content); ok {
			last.Set("content", append(append([]any(nil), blocks...), block))
		} else if truthy(content) {
			last.Set("content", []any{obj("type", "text", "text", content), block})
		} else {
			last.Set("content", []any{block})
		}
		return messages
	}
	return append(messages, obj("role", "user", "content", []any{block}))
}

func anthropicCreateCall(p *Providers, model, apiKey string, configuration any) callFunc {
	conf := or(configuration, obj())
	return func(ctx context.Context, system SystemPrompt, userMessage string, opts *Opts) (*Result, error) {
		useCache := truthy(member(conf, "cache_prompts")) && opts != nil && opts.Cache && system.structured
		effort := member(conf, "thinking_effort")
		useThinking := truthy(effort) && !isStr(effort, "off")

		var sys any
		if useCache {
			parts := []any{}
			if system.Static != "" {
				parts = append(parts, obj("type", "text", "text", system.Static, "cache_control", obj("type", "ephemeral")))
			}
			if system.Dynamic != "" {
				parts = append(parts, obj("type", "text", "text", system.Dynamic))
			}
			sys = parts
		} else {
			sys = FlattenPrompt(system)
		}

		headerObj := obj("Content-Type", "application/json", "x-api-key", apiKey, "anthropic-version", "2023-06-01")
		for _, kv := range spreadEntries(or(member(conf, "headers"), obj())) {
			headerObj.Set(kv[0].(string), kv[1])
		}
		var headers []header
		for _, k := range headerObj.Keys() {
			v, err := headerValue(headerObj.Value(k))
			if err != nil {
				return nil, err
			}
			headers = append(headers, header{k, v})
		}

		var messages []*object
		if opts != nil && len(opts.Messages) > 0 {
			var err error
			messages, err = translateMessagesToAnthropic(opts.Messages)
			if err != nil {
				return nil, err
			}
		} else {
			messages = []*object{obj("role", "user", "content", userMessage)}
		}
		if useCache && len(opts.Messages) > 0 {
			if err := markHistoryCacheBreakpoint(messages); err != nil {
				return nil, err
			}
		}
		if opts != nil && opts.TurnContext != "" {
			messages = appendTurnContext(messages, opts.TurnContext)
		}

		maxTokens, ok, err := asNumber(member(conf, "max_tokens"))
		if err != nil {
			return nil, err
		}
		if !ok || maxTokens == 0 {
			maxTokens = 4096
		}
		msgs := make([]any, len(messages))
		for i, m := range messages {
			msgs[i] = m
		}
		body := obj("model", model, "max_tokens", maxTokens, "system", sys, "messages", msgs)

		thinkingOff := isStr(effort, "off")
		switch {
		case anthropicThinkingAlwaysOn[model]:
			// A stale "off" becomes the lowest effort rather than a 400; no
			// setting sends no effort, so Anthropic's default applies.
			body.Set("thinking", obj("type", "adaptive"))
			if useThinking {
				body.Set("output_config", obj("effort", effort))
			} else if thinkingOff {
				body.Set("output_config", obj("effort", "low"))
			}
		case useThinking:
			body.Set("thinking", obj("type", "adaptive"))
			body.Set("output_config", obj("effort", effort))
		case anthropicThinkingOnByDefault[model]:
			if thinkingOff {
				body.Set("thinking", obj("type", "disabled"))
			}
		case !anthropicSamplingRejected[model]:
			t, ok, err := asNumber(member(conf, "temperature"))
			if err != nil {
				return nil, err
			}
			if ok {
				body.Set("temperature", t)
			}
		}

		if opts != nil && len(opts.Stop) > 0 {
			body.Set("stop_sequences", stringsToValues(opts.Stop, 4))
		}

		useTools := opts != nil && len(opts.Tools) > 0
		if useTools {
			tools := make([]any, len(opts.Tools))
			for i, tool := range opts.Tools {
				tools[i] = obj("name", tool.Value("name"), "description", tool.Value("description"), "input_schema", or(tool.Value("parameters"), defaultParameters()))
			}
			body.Set("tools", tools)
		}

		thinking := false
		if t, ok := body.Get("thinking"); ok {
			thinking = isStr(t.(*object).Value("type"), "adaptive")
		}
		p.logProvider("api-call", logger.Fields{{Key: "provider", Value: "anthropic"}, {Key: "model", Value: model}, {Key: "cached", Value: useCache}, {Key: "thinking", Value: thinking}, {Key: "tools", Value: useTools}})

		url, err := endpoint("ANTHROPIC_API_BASE", "/v1/messages")
		if err != nil {
			return nil, err
		}
		resp, err := p.postJSON(ctx, url, headers, body)
		if err != nil {
			return nil, err
		}
		if !resp.ok() {
			text := resp.text()
			p.logProvider("api-error", logger.Fields{{Key: "provider", Value: "anthropic"}, {Key: "model", Value: model}, {Key: "status", Value: resp.status}, {Key: "error", Value: text}})
			return nil, apiError("Anthropic", resp.status, text)
		}
		data, err := resp.json()
		if err != nil {
			return nil, err
		}
		content, err := get(data, "content")
		if err != nil {
			return nil, err
		}
		blocks, ok := arr(content)
		if !ok {
			if content == undefined || content == nil {
				return nil, readErr(content, "filter")
			}
			return nil, notFunction("data.content.filter")
		}
		var texts []any
		for _, block := range blocks {
			typ, err := get(block, "type")
			if err != nil {
				return nil, err
			}
			if isStr(typ, "text") {
				t, _ := get(block, "text")
				texts = append(texts, t)
			}
		}
		text, err := join(texts, "\n")
		if err != nil {
			return nil, err
		}
		toolCalls := []ToolCall{}
		for _, block := range blocks {
			typ, _ := get(block, "type")
			if isStr(typ, "tool_use") {
				id, _ := get(block, "id")
				name, _ := get(block, "name")
				input, _ := get(block, "input")
				toolCalls = append(toolCalls, ToolCall{ID: id, Name: name, Input: input})
			}
		}

		usageData := optional(data, "usage")
		usage := Usage{
			InputTokens:              or(optional(usageData, "input_tokens"), 0.0),
			OutputTokens:             or(optional(usageData, "output_tokens"), 0.0),
			CacheCreationInputTokens: or(optional(usageData, "cache_creation_input_tokens"), 0.0),
			CacheReadInputTokens:     or(optional(usageData, "cache_read_input_tokens"), 0.0),
		}
		stop, _ := get(data, "stop_reason")
		finish := NormalizeAnthropicStop(stop)

		fields := logger.Fields{{Key: "provider", Value: "anthropic"}, {Key: "model", Value: model}, {Key: "tool_calls", Value: len(toolCalls)}, {Key: "finish_reason", Value: finish}}
		p.logProvider("api-response", append(fields, usage.fields()...))

		return &Result{Text: text, ToolCalls: toolCalls, Usage: usage, FinishReason: finish, Truncated: IsTruncated(finish)}, nil
	}
}

// defaultParameters is the empty object schema sent for a tool without
// parameters.
func defaultParameters() *object {
	return obj("type", "object", "properties", obj())
}

// stringsToValues is stop.slice(0, n) as JSON values.
func stringsToValues(s []string, n int) []any {
	if len(s) > n {
		s = s[:n]
	}
	out := make([]any, len(s))
	for i, x := range s {
		out[i] = x
	}
	return out
}
