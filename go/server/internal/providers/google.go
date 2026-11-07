package providers

import (
	"context"
	"regexp"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

const (
	googleTemperatureDescription = "Controls randomness. Lower values are more focused and deterministic, higher values are more creative."
	googleTopPDescription        = "Nucleus sampling threshold. Considers tokens with cumulative probability up to this value. Lower values are more focused."
	googleTopKDescription        = "Limits token selection to the top K most probable tokens at each step. Lower values are more focused."
	googleThinkingMaxTokens      = "Maximum number of tokens the model will generate. Thinking tokens count against this limit when thinking is enabled."
	googleThinkingBudget         = "Maximum tokens for internal reasoning. Set to 0 to disable thinking entirely. The model auto-adjusts within this budget based on task complexity. Thinking tokens count against max output tokens."
)

func googleCapabilities(maxTokensDescription string, maxTokens float64, thinking bool) Capabilities {
	caps := Capabilities{
		stepCap("temperature", "Temperature", googleTemperatureDescription, 1.0, 0, 2.0, 0.1),
		numberCap("max_tokens", "Max Output Tokens", maxTokensDescription, 8192, 1, maxTokens),
	}
	if thinking {
		caps = append(caps, numberCap("thinking_budget", "Thinking Budget (tokens)", googleThinkingBudget, 0, 0, 65536))
	}
	return append(caps,
		stepCap("top_p", "Top P", googleTopPDescription, 0.95, 0, 1.0, 0.05),
		numberCap("top_k", "Top K", googleTopKDescription, 40, 1, 100),
	)
}

var googleModels = Models{
	{ID: "gemini-2.5-pro", Label: "Gemini 2.5 Pro", ConfigVersion: 1, Pricing: price(1.25, 10), Capabilities: googleCapabilities(googleThinkingMaxTokens, 65536, true)},
	{ID: "gemini-2.5-flash", Label: "Gemini 2.5 Flash", ConfigVersion: 1, Pricing: price(0.15, 0.60), Capabilities: googleCapabilities(googleThinkingMaxTokens, 65536, true)},
	{ID: "gemini-2.5-flash-lite", Label: "Gemini 2.5 Flash-Lite", ConfigVersion: 1, Pricing: price(0.075, 0.30), Capabilities: googleCapabilities("Maximum number of tokens the model will generate.", 65536, false)},
	{ID: "gemini-2.0-flash", Label: "Gemini 2.0 Flash", ConfigVersion: 1, Deprecated: "Retiring June 1, 2026. Migrate to Gemini 2.5 Flash.", Pricing: price(0.10, 0.40), Capabilities: googleCapabilities("Maximum number of tokens the model will generate.", 8192, false)},
}

var googleAdapter = &adapter{
	name:       "google",
	label:      "Google",
	aliases:    []string{"gemini"},
	models:     googleModels,
	createCall: googleCreateCall,
}

var (
	geminiPrefix = regexp.MustCompile(`^gemini-`)
	indexSuffix  = regexp.MustCompile(`-[0-9]+$`)
)

// translateMessagesToGoogle turns OpenAI-shape messages into Gemini
// contents: assistant tool calls become functionCall parts of a "model"
// turn, and a tool result a "function" turn whose functionResponse name
// comes from the synthetic gemini-<name>-<n> call id. System messages are
// dropped (the prompt is system_instruction).
func translateMessagesToGoogle(messages []*object) ([]any, error) {
	out := []any{}
	for _, msg := range messages {
		// SanitizeOpts kept only messages with a string role.
		role, _ := msg.Value("role").(string)
		switch role {
		case "tool":
			id := or(msg.Value("tool_call_id"), "")
			s, ok := id.(string)
			if !ok {
				return nil, notFunction(`(msg.tool_call_id || "").replace`)
			}
			name := indexSuffix.ReplaceAllString(geminiPrefix.ReplaceAllString(s, ""), "")
			if name == "" {
				name = "unknown"
			}
			content := msg.Value("content")
			if !isString(content) {
				content = stringifyValue(content)
			}
			out = append(out, obj("role", "function", "parts", []any{obj("functionResponse", obj("name", name, "response", obj("content", content)))}))
		case "user":
			out = append(out, obj("role", "user", "parts", []any{obj("text", or(msg.Value("content"), ""))}))
		case "assistant":
			parts := []any{}
			if c := msg.Value("content"); truthy(c) {
				parts = append(parts, obj("text", c))
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
					var args any = obj()
					a, _ := get(fn, "arguments")
					if s, ok := a.(string); ok {
						if parsed, err := parseJSON(s); err == nil {
							args = parsed
						}
					} else if truthy(a) && typeofObject(a) {
						args = a
					}
					parts = append(parts, obj("functionCall", obj("name", name, "args", args)))
				}
			}
			if len(parts) == 0 {
				parts = []any{obj("text", "")}
			}
			out = append(out, obj("role", "model", "parts", parts))
		}
	}
	return out, nil
}

func googleCreateCall(p *Providers, model, apiKey string, configuration any) callFunc {
	conf := or(configuration, obj())
	return func(ctx context.Context, system SystemPrompt, userMessage string, opts *Opts) (*Result, error) {
		prompt := FlattenPrompt(system)
		url, err := endpoint("GOOGLE_API_BASE", "/v1beta/models/"+model+":generateContent")
		if err != nil {
			return nil, err
		}

		var contents []any
		if opts != nil && len(opts.Messages) > 0 {
			contents, err = translateMessagesToGoogle(opts.Messages)
			if err != nil {
				return nil, err
			}
		} else {
			contents = []any{obj("role", "user", "parts", []any{obj("text", userMessage)})}
		}

		gen := obj()
		body := obj("system_instruction", obj("parts", obj("text", prompt)), "contents", contents, "generationConfig", gen)

		for _, f := range []struct{ conf, key string }{
			{"max_tokens", "maxOutputTokens"}, {"temperature", "temperature"}, {"top_p", "topP"}, {"top_k", "topK"},
		} {
			n, ok, err := asNumber(member(conf, f.conf))
			if err != nil {
				return nil, err
			}
			if ok {
				gen.Set(f.key, n)
			}
		}
		// 0 disables thinking, a positive value caps it; unset leaves the
		// model's default.
		budget, ok, err := asNumber(member(conf, "thinking_budget"))
		if err != nil {
			return nil, err
		}
		if ok {
			gen.Set("thinkingConfig", obj("thinkingBudget", budget))
		}
		if opts != nil && len(opts.Stop) > 0 {
			gen.Set("stopSequences", stringsToValues(opts.Stop, 5))
		}

		useTools := opts != nil && len(opts.Tools) > 0
		if useTools {
			decls := make([]any, len(opts.Tools))
			for i, tool := range opts.Tools {
				decls[i] = obj("name", tool.Value("name"), "description", tool.Value("description"), "parameters", or(tool.Value("parameters"), defaultParameters()))
			}
			body.Set("tools", []any{obj("functionDeclarations", decls)})
		}

		p.logProvider("api-call", logger.Fields{{Key: "provider", Value: "google"}, {Key: "model", Value: model}, {Key: "tools", Value: useTools}})

		resp, err := p.postJSON(ctx, url, []header{{"Content-Type", "application/json"}, {"x-goog-api-key", apiKey}}, body)
		if err != nil {
			return nil, err
		}
		if !resp.ok() {
			text := resp.text()
			p.logProvider("api-error", logger.Fields{{Key: "provider", Value: "google"}, {Key: "model", Value: model}, {Key: "status", Value: resp.status}, {Key: "error", Value: text}})
			return nil, apiError("Gemini", resp.status, text)
		}
		data, err := resp.json()
		if err != nil {
			return nil, err
		}
		candidates, err := get(data, "candidates")
		if err != nil {
			return nil, err
		}
		candidate := candidates
		if truthy(candidates) {
			candidate = index0(candidates)
		}
		const noContent = errorString("Gemini API returned no content")
		if !truthy(candidate) {
			return nil, noContent
		}
		content, _ := get(candidate, "content")
		if !truthy(content) {
			return nil, noContent
		}
		partsValue, _ := get(content, "parts")
		if !truthy(partsValue) {
			return nil, noContent
		}
		parts, ok := arr(partsValue)
		if !ok {
			return nil, notFunction("candidate.content.parts.filter")
		}

		var texts []any
		for _, part := range parts {
			t, err := get(part, "text")
			if err != nil {
				return nil, err
			}
			if truthy(t) {
				texts = append(texts, t)
			}
		}
		text, err := join(texts, "\n")
		if err != nil {
			return nil, err
		}

		// Gemini sends no call id, so one is made from the name and the
		// call's position.
		toolCalls := []ToolCall{}
		for _, part := range parts {
			fc, _ := get(part, "functionCall")
			if !truthy(fc) {
				continue
			}
			name, _ := get(fc, "name")
			if !truthy(name) {
				continue
			}
			nameText, err := jsutil.ValueString(name)
			if err != nil {
				return nil, err
			}
			args, _ := get(fc, "args")
			toolCalls = append(toolCalls, ToolCall{
				ID:    "gemini-" + nameText + "-" + jsutil.NumberString(float64(len(toolCalls))),
				Name:  name,
				Input: or(args, obj()),
			})
		}

		// Thinking tokens (thoughtsTokenCount) are billed at the output
		// rate, so they are added in; only when both are numbers, as Node's
		// typeof checks.
		meta := optional(data, "usageMetadata")
		outputTokens := or(optional(meta, "candidatesTokenCount"), 0.0)
		if candidates, ok := jsutil.AsFloat(outputTokens); ok {
			if thoughts, ok := jsutil.AsFloat(optional(meta, "thoughtsTokenCount")); ok {
				outputTokens = candidates + thoughts
			}
		}
		usage := Usage{
			InputTokens:  or(optional(meta, "promptTokenCount"), 0.0),
			OutputTokens: outputTokens,
		}
		finishValue, _ := get(candidate, "finishReason")
		finish := NormalizeGoogleFinish(finishValue)

		fields := logger.Fields{{Key: "provider", Value: "google"}, {Key: "model", Value: model}, {Key: "tool_calls", Value: len(toolCalls)}, {Key: "finish_reason", Value: finish}}
		p.logProvider("api-response", append(fields, usage.fields()...))

		return &Result{Text: text, ToolCalls: toolCalls, Usage: usage, FinishReason: finish, Truncated: IsTruncated(finish)}, nil
	}
}
