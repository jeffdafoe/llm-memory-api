package providers

import (
	"context"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// Perplexity: Sonar models with built-in web search, on an
// OpenAI-compatible chat API. They take no tools and no conversation; a
// call is the system prompt and one user message. The sources the search
// used are appended to the text.

func perplexityCapabilities(maxTokensDefault, maxTokensMax float64, recency bool) Capabilities {
	caps := Capabilities{
		stepCap("temperature", "Temperature", "Controls randomness. Lower values are more focused and deterministic, higher values are more creative.", 0.2, 0, 2.0, 0.1),
		numberCap("max_tokens", "Max Output Tokens", "Maximum number of tokens the model will generate in its response.", maxTokensDefault, 1, maxTokensMax),
	}
	if recency {
		caps = append(caps, selectCap("search_recency_filter", "Search Recency", "Filter search results by recency. Leave unset for all time.", "", "", "hour", "day", "week", "month"))
	}
	return append(caps, boolCap("return_citations", "Include Citations", "Append source URLs to the response text.", true))
}

func perplexityPricing(input, output, request float64) *Pricing {
	return &Pricing{Input: num(input), Output: num(output), Request: num(request)}
}

var perplexityModels = Models{
	// The request fee is dollars per thousand requests, added to every call.
	{ID: "sonar", Label: "Sonar", ConfigVersion: 1, Pricing: perplexityPricing(1, 1, 5), Capabilities: perplexityCapabilities(1024, 4096, true)},
	{ID: "sonar-pro", Label: "Sonar Pro", ConfigVersion: 1, Pricing: perplexityPricing(3, 15, 6), Capabilities: perplexityCapabilities(4096, 8192, true)},
	{ID: "sonar-deep-research", Label: "Sonar Deep Research", ConfigVersion: 1, Pricing: perplexityPricing(2, 8, 5), Capabilities: perplexityCapabilities(4096, 8192, false)},
}

var perplexityAdapter = &adapter{
	name:       "perplexity",
	label:      "Perplexity",
	models:     perplexityModels,
	createCall: perplexityCreateCall,
}

// templateString is `${v}` in a template literal.
func templateString(v any) (string, error) {
	return jsutil.ValueString(v)
}

func perplexityCreateCall(p *Providers, model, apiKey string, configuration any) callFunc {
	conf := or(configuration, obj())
	return func(ctx context.Context, system SystemPrompt, userMessage string, opts *Opts) (*Result, error) {
		body := obj("model", model, "messages", []any{
			obj("role", "system", "content", FlattenPrompt(system)),
			obj("role", "user", "content", userMessage),
		})
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
		if recency := member(conf, "search_recency_filter"); truthy(recency) {
			body.Set("search_recency_filter", recency)
		}
		if opts != nil && len(opts.Stop) > 0 {
			body.Set("stop", stringsToValues(opts.Stop, 4))
		}
		if opts != nil && len(opts.Tools) > 0 {
			p.logProvider("tools-not-supported", logger.Fields{{Key: "provider", Value: "perplexity"}, {Key: "model", Value: model}, {Key: "requested", Value: len(opts.Tools)}})
		}

		p.logProvider("api-call", logger.Fields{{Key: "provider", Value: "perplexity"}, {Key: "model", Value: model}})

		url, err := endpoint("PERPLEXITY_API_BASE", "/chat/completions")
		if err != nil {
			return nil, err
		}
		resp, err := p.postJSON(ctx, url, []header{{"Content-Type", "application/json"}, {"Authorization", "Bearer " + apiKey}}, body)
		if err != nil {
			return nil, err
		}
		if !resp.ok() {
			text := resp.text()
			p.logProvider("api-error", logger.Fields{{Key: "provider", Value: "perplexity"}, {Key: "model", Value: model}, {Key: "status", Value: resp.status}, {Key: "error", Value: text}})
			return nil, apiError("Perplexity", resp.status, text)
		}
		data, err := resp.json()
		if err != nil {
			return nil, err
		}
		choice, message, err := firstChoice(data, "Perplexity API returned no content")
		if err != nil {
			return nil, err
		}
		text, _ := get(message, "content")

		// search_results ({title, url, date}) replaced the plain citations
		// list in May 2025; the legacy list is still read. Numbering is
		// 1-based to match the model's inline [n] markers.
		if member(conf, "return_citations") != false {
			var lines []string
			results, _ := get(data, "search_results")
			citations, _ := get(data, "citations")
			if list, ok := arr(results); ok && len(list) > 0 {
				for i, r := range list {
					title, err := get(r, "title")
					if err != nil {
						return nil, err
					}
					titlePart := ""
					if truthy(title) {
						s, err := templateString(title)
						if err != nil {
							return nil, err
						}
						titlePart = s + " — "
					}
					date, _ := get(r, "date")
					datePart := ""
					if truthy(date) {
						s, err := templateString(date)
						if err != nil {
							return nil, err
						}
						datePart = " (" + s + ")"
					}
					u, _ := get(r, "url")
					urlPart, err := templateString(or(u, ""))
					if err != nil {
						return nil, err
					}
					lines = append(lines, "["+jsutil.NumberString(float64(i+1))+"] "+titlePart+urlPart+datePart)
				}
			} else if list, ok := arr(citations); ok && len(list) > 0 {
				for i, u := range list {
					s, err := templateString(u)
					if err != nil {
						return nil, err
					}
					lines = append(lines, "["+jsutil.NumberString(float64(i+1))+"] "+s)
				}
			}
			if lines != nil {
				prefix, err := jsutil.ValueString(text)
				if err != nil {
					return nil, err
				}
				text = prefix + "\n\nSources:\n" + strings.Join(lines, "\n")
			}
		}

		usageData := optional(data, "usage")
		usage := Usage{
			InputTokens:  or(optional(usageData, "prompt_tokens"), 0.0),
			OutputTokens: or(optional(usageData, "completion_tokens"), 0.0),
		}
		finishValue, _ := get(choice, "finish_reason")
		finish := NormalizeOpenAIChatFinish(finishValue)

		fields := logger.Fields{{Key: "provider", Value: "perplexity"}, {Key: "model", Value: model}, {Key: "finish_reason", Value: finish}}
		p.logProvider("api-response", append(fields, usage.fields()...))

		return &Result{Text: text, ToolCalls: []ToolCall{}, Usage: usage, FinishReason: finish, Truncated: IsTruncated(finish)}, nil
	}
}
