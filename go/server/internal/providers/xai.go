package providers

import (
	"context"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// xAI: Grok models on the Responses API (/v1/responses), with optional
// server-side X and web search ($5 per thousand searches). A call is the
// system prompt and one user message; user function tools are not sent.

func xaiCapabilities(temperature bool) Capabilities {
	var caps Capabilities
	if temperature {
		caps = append(caps, stepCap("temperature", "Temperature", "Controls randomness. Lower values are more focused, higher values are more creative.", 1.0, 0, 2.0, 0.1))
	}
	return append(caps,
		boolCap("x_search", "X Search", "Enable searching X (Twitter) posts. $5 per 1,000 search calls.", false),
		boolCap("web_search", "Web Search", "Enable searching the web. $5 per 1,000 search calls.", false),
		numberCap("max_output_tokens", "Max Output Tokens", "Maximum number of tokens the model will generate in its response.", 8192, 1, 131072),
	)
}

var xaiModels = Models{
	{ID: "grok-4.20-reasoning", Label: "Grok 4.20", APIID: "grok-4.20-0309-reasoning", ConfigVersion: 1, Pricing: openaiPricing(2, 6, 0.20), Capabilities: xaiCapabilities(false)},
	{ID: "grok-4.20-non-reasoning", Label: "Grok 4.20 (non-reasoning)", APIID: "grok-4.20-0309-non-reasoning", ConfigVersion: 1, Pricing: openaiPricing(2, 6, 0.20), Capabilities: xaiCapabilities(true)},
	{ID: "grok-4.1-fast-reasoning", Label: "Grok 4.1 Fast", APIID: "grok-4-1-fast-reasoning", ConfigVersion: 1, Pricing: openaiPricing(0.20, 0.50, 0.02), Capabilities: xaiCapabilities(false)},
	{ID: "grok-4.1-fast-non-reasoning", Label: "Grok 4.1 Fast (non-reasoning)", APIID: "grok-4-1-fast-non-reasoning", ConfigVersion: 1, Pricing: openaiPricing(0.20, 0.50, 0.02), Capabilities: xaiCapabilities(true)},
}

var xaiAdapter = &adapter{
	name:          "xai",
	label:         "xAI",
	models:        xaiModels,
	createCall:    xaiCreateCall,
	formatPricing: xaiFormatPricing,
}

// xaiComputeCost finds the model by id or by API id (the call has the API
// id). Cached input is subtracted without a floor, as in Node.
func xaiComputeCost(model string, promptTokens, cachedTokens, completionTokens any) *float64 {
	entry := xaiModels.Get(model)
	if entry == nil {
		for _, m := range xaiModels {
			if m.APIID == model {
				entry = m
				break
			}
		}
	}
	if entry == nil || entry.Pricing == nil {
		return nil
	}
	pricing := entry.Pricing
	cached := toNumber(cachedTokens)
	uncached := toNumber(promptTokens) - cached
	cost := 0.0
	cost += uncached * pricing.rate(pricing.Input) / 1_000_000
	cost += cached * pricing.rate(pricing.CacheRead) / 1_000_000
	cost += toNumber(completionTokens) * pricing.rate(pricing.Output) / 1_000_000
	return &cost
}

// iterate is for...of over v: an array's elements or a string's code units.
func iterate(v any, expr string) ([]any, error) {
	switch x := v.(type) {
	case []any:
		return x, nil
	case string:
		units := utf16Units(x)
		out := make([]any, len(units))
		for i, u := range units {
			out[i] = u
		}
		return out, nil
	}
	return nil, notIterable(expr)
}

func xaiCreateCall(p *Providers, model, apiKey string, configuration any) callFunc {
	conf := or(configuration, obj())
	return func(ctx context.Context, system SystemPrompt, userMessage string, opts *Opts) (*Result, error) {
		body := obj("model", model, "input", []any{
			obj("role", "system", "content", FlattenPrompt(system)),
			obj("role", "user", "content", userMessage),
		})
		maxTokens, ok, err := asNumber(member(conf, "max_output_tokens"))
		if err != nil {
			return nil, err
		}
		if ok {
			body.Set("max_output_tokens", maxTokens)
		}
		temperature, ok, err := asNumber(member(conf, "temperature"))
		if err != nil {
			return nil, err
		}
		if ok {
			body.Set("temperature", temperature)
		}
		if opts != nil && len(opts.Stop) > 0 {
			body.Set("stop", stringsToValues(opts.Stop, 4))
		}
		if opts != nil && len(opts.Tools) > 0 {
			p.logProvider("tools-not-implemented", logger.Fields{{Key: "provider", Value: "xai"}, {Key: "model", Value: model}, {Key: "requested", Value: len(opts.Tools)}})
		}

		var tools []any
		var search []any
		if truthy(member(conf, "x_search")) {
			tools = append(tools, obj("type", "x_search"))
			search = append(search, "x_search")
		}
		if truthy(member(conf, "web_search")) {
			tools = append(tools, obj("type", "web_search"))
			search = append(search, "web_search")
		}
		if len(tools) > 0 {
			body.Set("tools", tools)
		}
		var searchLog any = false
		if len(search) > 0 {
			searchLog = search
		}
		p.logProvider("api-call", logger.Fields{{Key: "provider", Value: "xai"}, {Key: "model", Value: model}, {Key: "search", Value: searchLog}})

		url, err := endpoint("XAI_API_BASE", "/v1/responses")
		if err != nil {
			return nil, err
		}
		resp, err := p.postJSON(ctx, url, []header{{"Content-Type", "application/json"}, {"Authorization", "Bearer " + apiKey}}, body)
		if err != nil {
			return nil, err
		}
		if !resp.ok() {
			text := resp.text()
			p.logProvider("api-error", logger.Fields{{Key: "provider", Value: "xai"}, {Key: "model", Value: model}, {Key: "status", Value: resp.status}, {Key: "error", Value: text}})
			return nil, apiError("xAI", resp.status, text)
		}
		data, err := resp.json()
		if err != nil {
			return nil, err
		}

		text := ""
		appendText := func(v any) error {
			s, err := jsutil.ValueString(v)
			if err != nil {
				return err
			}
			if text != "" {
				text += "\n"
			}
			text += s
			return nil
		}
		output, err := get(data, "output")
		if err != nil {
			return nil, err
		}
		if items, ok := arr(output); ok {
			for _, item := range items {
				typ, err := get(item, "type")
				if err != nil {
					return nil, err
				}
				if isStr(typ, "output_text") {
					t, _ := get(item, "text")
					if err := appendText(t); err != nil {
						return nil, err
					}
				}
				content, _ := get(item, "content")
				if isStr(typ, "message") && truthy(content) {
					blocks, err := iterate(content, "item.content")
					if err != nil {
						return nil, err
					}
					for _, block := range blocks {
						bt, err := get(block, "type")
						if err != nil {
							return nil, err
						}
						if isStr(bt, "output_text") || isStr(bt, "text") {
							t, _ := get(block, "text")
							if err := appendText(or(t, "")); err != nil {
								return nil, err
							}
						}
					}
				}
			}
		}
		if text == "" {
			return nil, errorString("xAI API returned no content")
		}

		citations, _ := get(data, "citations")
		if truthy(citations) {
			length, _ := get(citations, "length")
			if toNumber(length) > 0 {
				list, ok := arr(citations)
				if !ok {
					return nil, notFunction("data.citations.map")
				}
				lines := make([]string, len(list))
				for i, c := range list {
					var u any = c
					var title string
					if _, isStr := c.(string); !isStr {
						cu, err := get(c, "url")
						if err != nil {
							return nil, err
						}
						u = or(cu, c)
						if t := member(c, "title"); typeofObject(c) && truthy(t) {
							s, err := templateString(t)
							if err != nil {
								return nil, err
							}
							title = " — " + s
						}
					}
					us, err := templateString(u)
					if err != nil {
						return nil, err
					}
					lines[i] = "[" + jsutil.NumberString(float64(i+1)) + "] " + us + title
				}
				text += "\n\nSources:\n" + strings.Join(lines, "\n")
			}
		}

		usageData := optional(data, "usage")
		promptTokens := or(optional(usageData, "input_tokens"), 0.0)
		completionTokens := or(optional(usageData, "output_tokens"), 0.0)
		cachedTokens := or(or(optional(usageData, "cache_read_input_tokens"), optional(optional(usageData, "input_tokens_details"), "cached_tokens")), 0.0)
		uncached := maxNum(0, toNumber(promptTokens)-toNumber(cachedTokens))
		usage := Usage{InputTokens: uncached, OutputTokens: completionTokens, CacheReadInputTokens: cachedTokens}
		cost := xaiComputeCost(model, promptTokens, cachedTokens, completionTokens)
		usage.Cost = cost

		status, _ := get(data, "status")
		finish := NormalizeResponsesStatus(status, optional(optional(data, "incomplete_details"), "reason"), false)

		p.logProvider("api-response", logger.Fields{
			{Key: "provider", Value: "xai"}, {Key: "model", Value: model},
			{Key: "input", Value: uncached}, {Key: "cached", Value: logValue(cachedTokens)},
			{Key: "output", Value: logValue(completionTokens)},
			{Key: "cost", Value: costLog(cost, 6)},
			{Key: "search", Value: searchLog},
			{Key: "finish_reason", Value: finish},
		})

		return &Result{Text: text, ToolCalls: []ToolCall{}, Usage: usage, FinishReason: finish, Truncated: IsTruncated(finish)}, nil
	}
}

// xaiFormatPricing lists the rates and the search tools turned on.
func xaiFormatPricing(p *Providers, modelID string, conf any) string {
	entry := xaiModels.Get(modelID)
	if entry == nil || entry.Pricing == nil {
		return "No pricing data"
	}
	var parts []string
	add := func(v *float64, suffix string) {
		if v != nil {
			parts = append(parts, "$"+jsutil.NumberString(*v)+suffix)
		}
	}
	add(entry.Pricing.Input, " in")
	add(entry.Pricing.Output, " out")
	add(entry.Pricing.CacheRead, " cached")
	result := strings.Join(parts, " / ") + " per 1M tokens"
	var search []string
	if truthy(conf) && truthy(member(conf, "x_search")) {
		search = append(search, "X")
	}
	if truthy(conf) && truthy(member(conf, "web_search")) {
		search = append(search, "web")
	}
	if len(search) > 0 {
		result += " + $5/1K " + strings.Join(search, "+") + " searches"
	}
	return result
}
