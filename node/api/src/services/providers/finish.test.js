// Tests for the finish-reason normalizers (LLM-418). Run with: node --test
// (from node/api). These are pure functions — no fetch stub needed. They map
// each provider's native stop signal onto the shared vocabulary, so the whole
// truncation-guard chain hinges on these tables being right.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const {
    FINISH,
    normalizeOpenAIChatFinish,
    normalizeResponsesStatus,
    normalizeAnthropicStop,
    normalizeGoogleFinish,
    isTruncated,
} = require('./finish');

test('OpenAI chat finish_reason maps to the shared vocabulary', () => {
    assert.equal(normalizeOpenAIChatFinish('stop'), FINISH.STOP);
    assert.equal(normalizeOpenAIChatFinish('length'), FINISH.LENGTH);
    assert.equal(normalizeOpenAIChatFinish('tool_calls'), FINISH.TOOL_CALLS);
    assert.equal(normalizeOpenAIChatFinish('function_call'), FINISH.TOOL_CALLS);
    assert.equal(normalizeOpenAIChatFinish('content_filter'), FINISH.CONTENT_FILTER);
    // Unknown / missing signal falls back to 'other', never 'length'.
    assert.equal(normalizeOpenAIChatFinish(undefined), FINISH.OTHER);
    assert.equal(normalizeOpenAIChatFinish(null), FINISH.OTHER);
    assert.equal(normalizeOpenAIChatFinish('surprise'), FINISH.OTHER);
});

test('/v1/responses status + incomplete_details maps correctly', () => {
    // Incomplete because of the token ceiling is the truncation case.
    assert.equal(normalizeResponsesStatus('incomplete', 'max_output_tokens', false), FINISH.LENGTH);
    assert.equal(normalizeResponsesStatus('incomplete', 'content_filter', false), FINISH.CONTENT_FILTER);
    assert.equal(normalizeResponsesStatus('incomplete', 'something_else', false), FINISH.OTHER);
    // Completed is a normal stop, unless a function_call rode along.
    assert.equal(normalizeResponsesStatus('completed', undefined, false), FINISH.STOP);
    assert.equal(normalizeResponsesStatus('completed', undefined, true), FINISH.TOOL_CALLS);
    // Any non-completed / non-incomplete status is NOT a clean stop — the
    // normalizer is the persistence safety boundary, so failed/cancelled/
    // unknown/missing all fall through to 'other', never 'stop'.
    assert.equal(normalizeResponsesStatus('failed', undefined, false), FINISH.OTHER);
    assert.equal(normalizeResponsesStatus('cancelled', undefined, false), FINISH.OTHER);
    assert.equal(normalizeResponsesStatus('in_progress', undefined, false), FINISH.OTHER);
    assert.equal(normalizeResponsesStatus('surprise_status', undefined, false), FINISH.OTHER);
    assert.equal(normalizeResponsesStatus(undefined, undefined, false), FINISH.OTHER);
});

test('Anthropic stop_reason maps to the shared vocabulary', () => {
    assert.equal(normalizeAnthropicStop('end_turn'), FINISH.STOP);
    assert.equal(normalizeAnthropicStop('stop_sequence'), FINISH.STOP);
    assert.equal(normalizeAnthropicStop('max_tokens'), FINISH.LENGTH);
    assert.equal(normalizeAnthropicStop('tool_use'), FINISH.TOOL_CALLS);
    assert.equal(normalizeAnthropicStop('refusal'), FINISH.CONTENT_FILTER);
    assert.equal(normalizeAnthropicStop('pause_turn'), FINISH.OTHER);
    assert.equal(normalizeAnthropicStop(undefined), FINISH.OTHER);
});

test('Google finishReason maps to the shared vocabulary', () => {
    assert.equal(normalizeGoogleFinish('STOP'), FINISH.STOP);
    assert.equal(normalizeGoogleFinish('MAX_TOKENS'), FINISH.LENGTH);
    assert.equal(normalizeGoogleFinish('SAFETY'), FINISH.CONTENT_FILTER);
    assert.equal(normalizeGoogleFinish('RECITATION'), FINISH.CONTENT_FILTER);
    assert.equal(normalizeGoogleFinish('PROHIBITED_CONTENT'), FINISH.CONTENT_FILTER);
    assert.equal(normalizeGoogleFinish('OTHER'), FINISH.OTHER);
    assert.equal(normalizeGoogleFinish(undefined), FINISH.OTHER);
});

test('isTruncated is true only for a length-stop', () => {
    assert.equal(isTruncated(FINISH.LENGTH), true);
    for (const r of [FINISH.STOP, FINISH.TOOL_CALLS, FINISH.CONTENT_FILTER, FINISH.OTHER, undefined]) {
        assert.equal(isTruncated(r), false);
    }
});
