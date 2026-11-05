package providers

import (
	"context"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// OpenAI: gpt-5.x and gpt-6.x go through /v1/responses (function tools are
// refused on /v1/chat/completions for them); any other model id through
// /v1/chat/completions. Cost is computed here because it depends on the
// service tier OpenAI applied (flex is half price), cached input, and the
// gpt-6.x long-context surcharge.

func openaiTemperature() Capability {
	c := stepCap("temperature", "Temperature", "Controls randomness. Lower values are more focused and deterministic, higher values are more creative.", 1.0, 0, 2.0, 0.1)
	c.DisabledWhen = &DisabledWhen{Field: "reasoning_effort", Condition: "notEquals", Value: "none", Message: "Not supported when reasoning is active"}
	return c
}

func openaiMaxTokens() Capability {
	return numberCap("max_completion_tokens", "Max Output Tokens", "Maximum number of tokens the model will generate in its response.", 8192, 1, 32768)
}

func openaiServiceTier() Capability {
	return selectCap("service_tier", "Service Tier", "Processing tier. \"auto\" uses standard pricing. \"flex\" is half the cost but higher latency — good for background/batch work.", "auto", "auto", "flex")
}

func gpt5Capabilities() Capabilities {
	return Capabilities{
		openaiTemperature(),
		selectCap("reasoning_effort", "Reasoning Effort", "Controls how much time the model spends thinking. Supports none (default), low, medium, high, and xhigh.", "none", "none", "low", "medium", "high", "xhigh"),
		openaiMaxTokens(),
		openaiServiceTier(),
	}
}

// gpt-6.x: OpenAI's default effort is medium, the levels vary per model,
// and there is no temperature (the responses path never sends it).
var (
	gpt6ReasoningAll    = []string{"none", "low", "medium", "high", "xhigh", "max"}
	gpt6ReasoningNoNone = []string{"low", "medium", "high", "xhigh", "max"}
)

func gpt6Capabilities(levels []string) Capabilities {
	return Capabilities{
		selectCap("reasoning_effort", "Reasoning Effort", "Controls how much time the model spends thinking. Supports "+strings.Join(levels, ", ")+". OpenAI default is medium.", "medium", append([]string(nil), levels...)...),
		openaiMaxTokens(),
		openaiServiceTier(),
	}
}

// gpt6LongContext: above 272,000 prompt tokens the whole request bills
// input and cached input at 2x, output at 1.5x.
func gpt6LongContext() *LongContext {
	return &LongContext{Threshold: 272000, Input: 2, CacheRead: 2, Output: 1.5}
}

func openaiPricing(input, output, cacheRead float64) *Pricing {
	return &Pricing{Input: num(input), Output: num(output), CacheRead: num(cacheRead)}
}

var openaiModels = Models{
	{ID: "gpt-6.1-sol", Label: "GPT-6.1 Sol", ConfigVersion: 3, Pricing: openaiPricing(2.00, 10, 0.10), LongContext: gpt6LongContext(), Capabilities: gpt6Capabilities(gpt6ReasoningNoNone)},
	{ID: "gpt-6.1-terra", Label: "GPT-6.1 Terra", ConfigVersion: 3, Pricing: openaiPricing(1.00, 5, 0.05), LongContext: gpt6LongContext(), Capabilities: gpt6Capabilities(gpt6ReasoningAll)},
	{ID: "gpt-6-luna", Label: "GPT-6 Luna", ConfigVersion: 3, Pricing: openaiPricing(0.10, 0.50, 0.01), LongContext: gpt6LongContext(), Capabilities: gpt6Capabilities(gpt6ReasoningAll)},
	{ID: "gpt-5.6-sol", Label: "GPT-5.6 Sol", ConfigVersion: 2, Pricing: openaiPricing(5.00, 30, 0.50), Capabilities: gpt5Capabilities()},
	{ID: "gpt-5.6-terra", Label: "GPT-5.6 Terra", ConfigVersion: 2, Pricing: openaiPricing(2.50, 15, 0.25), Capabilities: gpt5Capabilities()},
	{ID: "gpt-5.6-luna", Label: "GPT-5.6 Luna", ConfigVersion: 2, Pricing: openaiPricing(1.00, 6, 0.10), Capabilities: gpt5Capabilities()},
	{ID: "gpt-5.5", Label: "GPT-5.5", ConfigVersion: 2, Pricing: openaiPricing(5.00, 30, 0.50), Capabilities: gpt5Capabilities()},
	{ID: "gpt-5.5-pro", Label: "GPT-5.5 Pro", ConfigVersion: 2, Pricing: openaiPricing(30, 180, 3), Capabilities: gpt5Capabilities()},
	{ID: "gpt-5.4", Label: "GPT-5.4", ConfigVersion: 2, Pricing: openaiPricing(2.50, 15, 0.25), Capabilities: gpt5Capabilities()},
	{ID: "gpt-5.4-mini", Label: "GPT-5.4 Mini", ConfigVersion: 2, Pricing: openaiPricing(0.75, 4.50, 0.075), Capabilities: gpt5Capabilities()},
	{ID: "gpt-5.4-nano", Label: "GPT-5.4 Nano", ConfigVersion: 2, Pricing: openaiPricing(0.20, 1.25, 0.02), Capabilities: gpt5Capabilities()},
	{ID: "gpt-5.4-pro", Label: "GPT-5.4 Pro", ConfigVersion: 2, Pricing: openaiPricing(30, 180, 3), Capabilities: gpt5Capabilities()},
}

var openaiAdapter = &adapter{
	name:          "openai",
	label:         "OpenAI",
	models:        openaiModels,
	createCall:    openaiCreateCall,
	formatPricing: openaiFormatPricing,
}

// o-series ids are reasoning models: developer role, reasoning_effort, no
// temperature.
func isReasoningModel(model string) bool {
	return strings.HasPrefix(model, "o")
}

func isGpt6(model string) bool {
	return strings.HasPrefix(model, "gpt-6")
}

func usesResponsesEndpoint(model string) bool {
	return strings.HasPrefix(model, "gpt-5.") || isGpt6(model)
}

// messagesToResponsesInput turns chat messages into /v1/responses input:
// assistant tool calls become function_call items and tool messages
// function_call_output items. /v1/responses refuses a call without its
// output or an output without its call, so unpaired ones are dropped.
func messagesToResponsesInput(messages []*object) ([]any, error) {
	paired := valueSet{}
	assistantIDs := valueSet{}
	for _, msg := range messages {
		role := msg.Value("role")
		if isStr(role, "tool") && truthy(msg.Value("tool_call_id")) {
			paired.add(msg.Value("tool_call_id"))
		}
		if isStr(role, "assistant") {
			if calls, ok := arr(msg.Value("tool_calls")); ok {
				for _, tc := range calls {
					if !truthy(tc) {
						continue
					}
					id, _ := get(tc, "id")
					if truthy(id) {
						assistantIDs.add(id)
					}
				}
			}
		}
	}

	input := []any{}
	for _, msg := range messages {
		role := msg.Value("role")
		if isStr(role, "tool") {
			id := msg.Value("tool_call_id")
			if !truthy(id) || !assistantIDs.has(id) {
				continue
			}
			content := msg.Value("content")
			if !isString(content) {
				content = stringifyValue(content)
			}
			input = append(input, obj("type", "function_call_output", "call_id", id, "output", content))
			continue
		}
		calls, isArr := arr(msg.Value("tool_calls"))
		if isStr(role, "assistant") && isArr && len(calls) > 0 {
			if c, ok := msg.Value("content").(string); ok && c != "" && jsutil.Trim(c) != "" {
				input = append(input, obj("role", "assistant", "content", c))
			}
			for _, tc := range calls {
				typ, err := get(tc, "type")
				if err != nil {
					return nil, err
				}
				fn, _ := get(tc, "function")
				if !isStr(typ, "function") || !truthy(fn) {
					continue
				}
				id, _ := get(tc, "id")
				if !paired.has(id) {
					continue
				}
				name, _ := get(fn, "name")
				args, _ := get(fn, "arguments")
				input = append(input, obj("type", "function_call", "call_id", id, "name", name, "arguments", or(args, "{}")))
			}
			continue
		}
		input = append(input, obj("role", role, "content", msg.Value("content")))
	}
	return input, nil
}

// openaiComputeCost: uncached input, cached input and output at the
// model's rates, halved for flex, multiplied above the long-context
// threshold. Nil when the model has no pricing.
func openaiComputeCost(model string, serviceTier any, promptTokens, cachedTokens, completionTokens any) (*float64, error) {
	entry := openaiModels.Get(model)
	if entry == nil || entry.Pricing == nil {
		return nil, nil
	}
	n := &numbers{}
	pricing := entry.Pricing
	flex := 1.0
	if isStr(serviceTier, "flex") {
		flex = 0.5
	}
	inputMul, cacheMul, outputMul := 1.0, 1.0, 1.0
	prompt := n.of(promptTokens)
	if lc := entry.LongContext; lc != nil && prompt > lc.Threshold {
		inputMul, cacheMul, outputMul = lc.Input, lc.CacheRead, lc.Output
	}
	cached := n.of(cachedTokens)
	uncached := prompt - cached
	inputRate := pricing.rate(pricing.Input) * flex * inputMul
	cacheRate := pricing.rate(pricing.CacheRead) * flex * cacheMul
	outputRate := pricing.rate(pricing.Output) * flex * outputMul

	cost := 0.0
	cost += uncached * inputRate / 1_000_000
	cost += cached * cacheRate / 1_000_000
	cost += n.of(completionTokens) * outputRate / 1_000_000
	if n.err != nil {
		return nil, n.err
	}
	return &cost, nil
}

// maxTokensOf is asNumber(max_completion_tokens) ?? asNumber(max_tokens).
func maxTokensOf(conf any) (float64, bool, error) {
	n, ok, err := asNumber(member(conf, "max_completion_tokens"))
	if err != nil || ok {
		return n, ok, err
	}
	return asNumber(member(conf, "max_tokens"))
}

// toolsFunctionWrapped is the chat-completions tool shape.
func toolsFunctionWrapped(tools []*object) []any {
	out := make([]any, len(tools))
	for i, tool := range tools {
		out[i] = obj("type", "function", "function", obj("name", tool.Value("name"), "description", tool.Value("description"), "parameters", or(tool.Value("parameters"), defaultParameters())))
	}
	return out
}

// costLog is cost.toFixed(n), or "unknown".
func costLog(cost *float64, digits int) string {
	if cost == nil {
		return "unknown"
	}
	return jsutil.ToFixed(*cost, digits)
}

// tokenPair is Math.max(0, Math.min(raw, prompt)) and Math.max(0, prompt -
// cached): cached input clamped to the prompt, and the uncached rest.
func tokenPair(prompt, rawCached any) (cached, uncached float64, err error) {
	n := &numbers{}
	raw := n.of(rawCached)
	p := n.of(prompt)
	cached = maxNum(0, minNum(raw, p))
	uncached = maxNum(0, p-cached)
	return cached, uncached, n.err
}

func openaiCreateCall(p *Providers, model, apiKey string, configuration any) callFunc {
	conf := or(configuration, obj())
	return func(ctx context.Context, system SystemPrompt, userMessage string, opts *Opts) (*Result, error) {
		prompt := FlattenPrompt(system)
		reasoning := isReasoningModel(model)
		systemRole := "system"
		if reasoning {
			systemRole = "developer"
		}
		full := []*object{obj("role", systemRole, "content", prompt)}
		if opts != nil && len(opts.Messages) > 0 {
			full = append(full, opts.Messages...)
		} else {
			full = append(full, obj("role", "user", "content", userMessage))
		}

		if usesResponsesEndpoint(model) {
			return openaiCallResponses(ctx, p, model, apiKey, conf, full, opts)
		}

		msgs := make([]any, len(full))
		for i, m := range full {
			msgs[i] = m
		}
		body := obj("model", model, "messages", msgs)

		maxTokens, ok, err := maxTokensOf(conf)
		if err != nil {
			return nil, err
		}
		if ok {
			body.Set("max_completion_tokens", maxTokens)
		}

		effort := member(conf, "reasoning_effort")
		if reasoning {
			if truthy(effort) {
				body.Set("reasoning_effort", effort)
			}
		} else if truthy(effort) && !isStr(effort, "none") {
			body.Set("reasoning_effort", effort)
		} else {
			t, ok, err := asNumber(member(conf, "temperature"))
			if err != nil {
				return nil, err
			}
			if ok {
				body.Set("temperature", t)
			}
		}

		requestedTier := or(member(conf, "service_tier"), "auto")
		if truthy(requestedTier) && !isStr(requestedTier, "auto") {
			body.Set("service_tier", requestedTier)
		}

		if opts != nil && len(opts.Stop) > 0 {
			body.Set("stop", stringsToValues(opts.Stop, 4))
		}
		useTools := opts != nil && len(opts.Tools) > 0
		if useTools {
			body.Set("tools", toolsFunctionWrapped(opts.Tools))
		}

		p.logProvider("api-call", logger.Fields{{Key: "provider", Value: "openai"}, {Key: "model", Value: model}, {Key: "reasoning", Value: reasoning}, {Key: "serviceTier", Value: logValue(requestedTier)}, {Key: "tools", Value: useTools}})

		url, err := endpoint("OPENAI_API_BASE", "/v1/chat/completions")
		if err != nil {
			return nil, err
		}
		resp, err := p.postJSON(ctx, url, []header{{"Content-Type", "application/json"}, {"Authorization", "Bearer " + apiKey}}, body)
		if err != nil {
			return nil, err
		}
		if !resp.ok() {
			text := resp.text()
			p.logProvider("api-error", logger.Fields{{Key: "provider", Value: "openai"}, {Key: "model", Value: model}, {Key: "status", Value: resp.status}, {Key: "error", Value: text}})
			return nil, apiError("OpenAI", resp.status, text)
		}
		data, err := resp.json()
		if err != nil {
			return nil, err
		}
		choice, message, err := firstChoice(data, "OpenAI API returned no content")
		if err != nil {
			return nil, err
		}

		usageData := optional(data, "usage")
		promptTokens := coalesce(optional(usageData, "prompt_tokens"), 0.0)
		completionTokens := coalesce(optional(usageData, "completion_tokens"), 0.0)
		rawCached := coalesce(optional(optional(usageData, "prompt_tokens_details"), "cached_tokens"), 0.0)
		cached, uncached, err := tokenPair(promptTokens, rawCached)
		if err != nil {
			return nil, err
		}

		serviceTier, _ := get(data, "service_tier")
		appliedTier := coalesce(serviceTier, requestedTier)
		usage := Usage{InputTokens: uncached, OutputTokens: completionTokens, CacheReadInputTokens: cached}
		cost, err := openaiComputeCost(model, appliedTier, promptTokens, cached, completionTokens)
		if err != nil {
			return nil, err
		}
		usage.Cost = cost

		toolCalls, err := chatToolCalls(p, "openai", model, "(choice.message.tool_calls || []).filter", message, false, opts)
		if err != nil {
			return nil, err
		}
		finishValue, _ := get(choice, "finish_reason")
		finish := NormalizeOpenAIChatFinish(finishValue)

		p.logProvider("api-response", logger.Fields{
			{Key: "provider", Value: "openai"}, {Key: "model", Value: model},
			{Key: "serviceTier", Value: logValue(appliedTier)},
			{Key: "input", Value: uncached}, {Key: "cached", Value: cached},
			{Key: "output", Value: logValue(completionTokens)}, {Key: "cost", Value: costLog(cost, 6)},
			{Key: "tool_calls", Value: len(toolCalls)}, {Key: "finish_reason", Value: finish},
		})

		content, _ := get(message, "content")
		return &Result{Text: or(content, ""), ToolCalls: toolCalls, Usage: usage, FinishReason: finish, Truncated: IsTruncated(finish)}, nil
	}
}

// firstChoice is `data.choices && data.choices[0]` and the check that it
// has a message.
func firstChoice(data any, noContent string) (choice, message any, err error) {
	choices, err := get(data, "choices")
	if err != nil {
		return nil, nil, err
	}
	choice = choices
	if truthy(choices) {
		choice = index0(choices)
	}
	if !truthy(choice) {
		return nil, nil, errorString(noContent)
	}
	message, _ = get(choice, "message")
	if !truthy(message) {
		return nil, nil, errorString(noContent)
	}
	return choice, message, nil
}

type errorString string

func (e errorString) Error() string {
	return string(e)
}

// chatToolCalls reads choice.message.tool_calls in the chat-completions
// shape into the neutral one, parsing each arguments string; unparseable
// arguments become {} and are logged. expr is how V8 prints the list
// expression when it is not an array. coerce (OpenRouter) coerces each
// call's input against its tool's schema; Node read opts.tools there, which
// fails when the call had no options.
func chatToolCalls(p *Providers, provider, model, expr string, message any, coerce bool, opts *Opts) ([]ToolCall, error) {
	raw, _ := get(message, "tool_calls")
	calls, ok := arr(or(raw, []any{}))
	if !ok {
		return nil, notFunction(expr)
	}
	type kept struct{ tc, fn, name any }
	var filtered []kept
	for _, tc := range calls {
		typ, err := get(tc, "type")
		if err != nil {
			return nil, err
		}
		if !isStr(typ, "function") {
			continue
		}
		fn, _ := get(tc, "function")
		if !truthy(fn) {
			continue
		}
		name, _ := get(fn, "name")
		if !truthy(name) {
			continue
		}
		filtered = append(filtered, kept{tc, fn, name})
	}
	out := []ToolCall{}
	for _, k := range filtered {
		var input any = obj()
		args, _ := get(k.fn, "arguments")
		if truthy(args) {
			parsed, err := parseJSON(args)
			if err != nil {
				p.logProvider("tool-args-parse-error", logger.Fields{{Key: "provider", Value: provider}, {Key: "model", Value: model}, {Key: "error", Value: err.Error()}})
			} else {
				input = parsed
			}
		}
		if coerce {
			if opts == nil {
				return nil, readErr(undefined, "tools")
			}
			for _, t := range opts.Tools {
				if sameValue(t.Value("name"), k.name) {
					input = CoerceToolArgs(input, t.Value("parameters"))
					break
				}
			}
		}
		id, _ := get(k.tc, "id")
		out = append(out, ToolCall{ID: id, Name: k.name, Input: input})
	}
	return out, nil
}

// sameValue is a === b for JSON values (objects never equal).
func sameValue(a, b any) bool {
	ka, ok1 := sameValueKey(a)
	kb, ok2 := sameValueKey(b)
	return ok1 && ok2 && ka == kb
}

func openaiCallResponses(ctx context.Context, p *Providers, model, apiKey string, conf any, full []*object, opts *Opts) (*Result, error) {
	input, err := messagesToResponsesInput(full)
	if err != nil {
		return nil, err
	}
	body := obj("model", model, "input", input)

	maxTokens, ok, err := maxTokensOf(conf)
	if err != nil {
		return nil, err
	}
	if ok {
		body.Set("max_output_tokens", maxTokens)
	}

	// gpt-6.x defaults to medium, so "none" is a real choice there and is
	// sent; gpt-5.x sends only a non-default level.
	effort := member(conf, "reasoning_effort")
	if isGpt6(model) {
		if truthy(effort) {
			body.Set("reasoning", obj("effort", effort))
		}
	} else if truthy(effort) && !isStr(effort, "none") {
		body.Set("reasoning", obj("effort", effort))
	}

	useTools := opts != nil && len(opts.Tools) > 0
	if useTools {
		tools := make([]any, len(opts.Tools))
		for i, tool := range opts.Tools {
			tools[i] = obj("type", "function", "name", tool.Value("name"), "description", tool.Value("description"), "parameters", or(tool.Value("parameters"), defaultParameters()))
		}
		body.Set("tools", tools)
	}

	p.logProvider("api-call", logger.Fields{{Key: "provider", Value: "openai"}, {Key: "model", Value: model}, {Key: "endpoint", Value: "responses"}, {Key: "tools", Value: useTools}})

	url, err := endpoint("OPENAI_API_BASE", "/v1/responses")
	if err != nil {
		return nil, err
	}
	resp, err := p.postJSON(ctx, url, []header{{"Content-Type", "application/json"}, {"Authorization", "Bearer " + apiKey}}, body)
	if err != nil {
		return nil, err
	}
	if !resp.ok() {
		text := resp.text()
		p.logProvider("api-error", logger.Fields{{Key: "provider", Value: "openai"}, {Key: "model", Value: model}, {Key: "endpoint", Value: "responses"}, {Key: "status", Value: resp.status}, {Key: "error", Value: text}})
		return nil, apiError("OpenAI", resp.status, text)
	}
	data, err := resp.json()
	if err != nil {
		return nil, err
	}
	outputValue, err := get(data, "output")
	if err != nil {
		return nil, err
	}
	output, ok := arr(outputValue)
	if !ok {
		return nil, errorString("OpenAI /v1/responses returned no output array")
	}

	text := ""
	toolCalls := []ToolCall{}
	for _, item := range output {
		typ, err := get(item, "type")
		if err != nil {
			return nil, err
		}
		content, _ := get(item, "content")
		if parts, ok := arr(content); ok && isStr(typ, "message") {
			for _, part := range parts {
				pt, err := get(part, "type")
				if err != nil {
					return nil, err
				}
				pText, _ := get(part, "text")
				if s, ok := pText.(string); ok && isStr(pt, "output_text") {
					text += s
				}
			}
			continue
		}
		name, _ := get(item, "name")
		if isStr(typ, "function_call") && truthy(name) {
			var in any = obj()
			args, _ := get(item, "arguments")
			if truthy(args) {
				parsed, err := parseJSON(args)
				if err != nil {
					p.logProvider("tool-args-parse-error", logger.Fields{{Key: "provider", Value: "openai"}, {Key: "model", Value: model}, {Key: "error", Value: err.Error()}})
				} else {
					in = parsed
				}
			}
			callID, _ := get(item, "call_id")
			toolCalls = append(toolCalls, ToolCall{ID: callID, Name: name, Input: in})
		}
	}

	usageData := optional(data, "usage")
	promptTokens := coalesce(optional(usageData, "input_tokens"), 0.0)
	completionTokens := coalesce(optional(usageData, "output_tokens"), 0.0)
	rawCached := coalesce(optional(optional(usageData, "input_tokens_details"), "cached_tokens"), 0.0)
	cached, uncached, err := tokenPair(promptTokens, rawCached)
	if err != nil {
		return nil, err
	}

	serviceTier, _ := get(data, "service_tier")
	appliedTier := coalesce(serviceTier, "default")
	usage := Usage{InputTokens: uncached, OutputTokens: completionTokens, CacheReadInputTokens: cached}
	cost, err := openaiComputeCost(model, appliedTier, promptTokens, cached, completionTokens)
	if err != nil {
		return nil, err
	}
	usage.Cost = cost

	status, _ := get(data, "status")
	finish := NormalizeResponsesStatus(status, optional(optional(data, "incomplete_details"), "reason"), len(toolCalls) > 0)

	p.logProvider("api-response", logger.Fields{
		{Key: "provider", Value: "openai"}, {Key: "model", Value: model},
		{Key: "endpoint", Value: "responses"},
		{Key: "serviceTier", Value: logValue(appliedTier)},
		{Key: "input", Value: uncached}, {Key: "cached", Value: cached},
		{Key: "output", Value: logValue(completionTokens)}, {Key: "cost", Value: costLog(cost, 6)},
		{Key: "tool_calls", Value: len(toolCalls)}, {Key: "finish_reason", Value: finish},
	})

	return &Result{Text: text, ToolCalls: toolCalls, Usage: usage, FinishReason: finish, Truncated: IsTruncated(finish)}, nil
}

// openaiFormatPricing lists the rates, halved and marked for flex.
func openaiFormatPricing(p *Providers, modelID string, conf any) string {
	entry := openaiModels.Get(modelID)
	if entry == nil || entry.Pricing == nil {
		return "No pricing data"
	}
	isFlex := truthy(conf) && isStr(member(conf, "service_tier"), "flex")
	m := 1.0
	if isFlex {
		m = 0.5
	}
	var parts []string
	add := func(v *float64, suffix string) {
		if v != nil {
			parts = append(parts, "$"+jsutil.NumberString(*v*m)+suffix)
		}
	}
	add(entry.Pricing.Input, " in")
	add(entry.Pricing.Output, " out")
	add(entry.Pricing.CacheRead, " cached")
	result := strings.Join(parts, " / ") + " per 1M tokens"
	if isFlex {
		result += " (flex tier)"
	}
	return result
}
