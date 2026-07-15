// Normalizes each provider's native stop/truncation signal into one vocabulary
// so callers don't have to know each API's shape. Every provider's call()
// returns a `finish_reason` (one of the strings below) plus a derived
// `truncated` boolean.
//
// Vocabulary:
//   'stop'           — model finished normally (end of turn / stop sequence)
//   'length'         — output hit the token ceiling (THE truncation case)
//   'tool_calls'     — model stopped to emit tool / function calls
//   'content_filter' — provider blocked or refused the output (safety, refusal)
//   'other'          — anything unrecognized, and the fallback when the signal
//                      is missing entirely
//
// `truncated === (finish_reason === 'length')`. Callers that persist model
// output (soul / people / learnings) treat a truncated generation as a failure
// and refuse to overwrite good state with a cut-off document.

const FINISH = {
    STOP: 'stop',
    LENGTH: 'length',
    TOOL_CALLS: 'tool_calls',
    CONTENT_FILTER: 'content_filter',
    OTHER: 'other',
};

// OpenAI Chat Completions shape — `choice.finish_reason`.
// Shared by openai (chat path), openrouter, and perplexity.
function normalizeOpenAIChatFinish(reason) {
    switch (reason) {
        case 'stop': return FINISH.STOP;
        case 'length': return FINISH.LENGTH;
        case 'tool_calls':
        case 'function_call': return FINISH.TOOL_CALLS;
        case 'content_filter': return FINISH.CONTENT_FILTER;
        default: return FINISH.OTHER;
    }
}

// OpenAI /v1/responses and xAI /v1/responses shape — top-level `status` plus
// `incomplete_details.reason`. A completed response is a normal stop unless it
// carries a function_call item, which the caller signals via hasToolCall.
function normalizeResponsesStatus(status, incompleteReason, hasToolCall) {
    if (status === 'incomplete') {
        if (incompleteReason === 'max_output_tokens') return FINISH.LENGTH;
        if (incompleteReason === 'content_filter') return FINISH.CONTENT_FILTER;
        return FINISH.OTHER;
    }
    if (status === 'completed') {
        return hasToolCall ? FINISH.TOOL_CALLS : FINISH.STOP;
    }
    // failed / cancelled / queued / in_progress / missing / any future status —
    // NOT a clean stop. This normalizer is the persistence safety boundary, so
    // never label an unrecognized status as success (a partial body on a failed
    // response must not read as a whole generation).
    return FINISH.OTHER;
}

// Anthropic Messages shape — top-level `stop_reason`.
function normalizeAnthropicStop(reason) {
    switch (reason) {
        case 'end_turn':
        case 'stop_sequence': return FINISH.STOP;
        case 'max_tokens': return FINISH.LENGTH;
        case 'tool_use': return FINISH.TOOL_CALLS;
        case 'refusal': return FINISH.CONTENT_FILTER;
        default: return FINISH.OTHER;
    }
}

// Google Gemini shape — `candidate.finishReason` (UPPER_SNAKE_CASE).
function normalizeGoogleFinish(reason) {
    switch (reason) {
        case 'STOP': return FINISH.STOP;
        case 'MAX_TOKENS': return FINISH.LENGTH;
        case 'SAFETY':
        case 'RECITATION':
        case 'BLOCKLIST':
        case 'PROHIBITED_CONTENT':
        case 'SPII': return FINISH.CONTENT_FILTER;
        default: return FINISH.OTHER;
    }
}

// True only for a length-stop — the actionable signal for the persist guards.
function isTruncated(finishReason) {
    return finishReason === FINISH.LENGTH;
}

module.exports = {
    FINISH,
    normalizeOpenAIChatFinish,
    normalizeResponsesStatus,
    normalizeAnthropicStop,
    normalizeGoogleFinish,
    isTruncated,
};
