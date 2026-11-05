package providers

// Finish reasons: each provider's stop signal mapped to one vocabulary
// (Node's providers/finish.js). Truncated is FinishLength only; callers that
// persist model output refuse a truncated generation.
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishToolCalls     = "tool_calls"
	FinishContentFilter = "content_filter"
	FinishOther         = "other"
)

// reason is the provider's value when it is a string; anything else matches
// no case, as in Node's switch.
func reason(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return "\x00"
}

// NormalizeOpenAIChatFinish maps choice.finish_reason (OpenAI chat,
// OpenRouter, Perplexity).
func NormalizeOpenAIChatFinish(v any) string {
	switch reason(v) {
	case "stop":
		return FinishStop
	case "length":
		return FinishLength
	case "tool_calls", "function_call":
		return FinishToolCalls
	case "content_filter":
		return FinishContentFilter
	}
	return FinishOther
}

// NormalizeResponsesStatus maps the /v1/responses status and
// incomplete_details.reason (OpenAI and xAI). Any status other than
// completed or incomplete is not a clean stop.
func NormalizeResponsesStatus(status, incompleteReason any, hasToolCall bool) string {
	switch reason(status) {
	case "incomplete":
		switch reason(incompleteReason) {
		case "max_output_tokens":
			return FinishLength
		case "content_filter":
			return FinishContentFilter
		}
		return FinishOther
	case "completed":
		if hasToolCall {
			return FinishToolCalls
		}
		return FinishStop
	}
	return FinishOther
}

// NormalizeAnthropicStop maps stop_reason.
func NormalizeAnthropicStop(v any) string {
	switch reason(v) {
	case "end_turn", "stop_sequence":
		return FinishStop
	case "max_tokens":
		return FinishLength
	case "tool_use":
		return FinishToolCalls
	case "refusal":
		return FinishContentFilter
	}
	return FinishOther
}

// NormalizeGoogleFinish maps candidate.finishReason.
func NormalizeGoogleFinish(v any) string {
	switch reason(v) {
	case "STOP":
		return FinishStop
	case "MAX_TOKENS":
		return FinishLength
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return FinishContentFilter
	}
	return FinishOther
}

// IsTruncated is true only for a length stop.
func IsTruncated(finishReason string) bool {
	return finishReason == FinishLength
}
