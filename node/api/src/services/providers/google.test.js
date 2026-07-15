// Tests for Gemini truncation detection (LLM-418) in createCall. Run with:
// node --test (from node/api). Gemini is the named casualty — thinking tokens
// count against maxOutputTokens, so a partial soul comes back with
// finishReason "MAX_TOKENS" while output_tokens looks healthy. Its response
// shape (candidate.finishReason) is distinct from the OpenAI/Anthropic ones,
// so it gets its own end-to-end assertion. Uses the built-in node:test runner
// + node:assert with a globalThis.fetch stub, matching the other provider tests.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const google = require('./google');

// Stub fetch to return a Gemini generateContent payload with the given
// finishReason and a partial text part.
function stubFinishReason(finishReason) {
    const original = globalThis.fetch;
    globalThis.fetch = async function () {
        return {
            ok: true,
            json: async () => ({
                candidates: [{
                    content: { parts: [{ text: 'partial' }] },
                    finishReason,
                }],
                usageMetadata: { promptTokenCount: 10, candidatesTokenCount: 4096 },
            }),
        };
    };
    return { restore() { globalThis.fetch = original; } };
}

test('finishReason "MAX_TOKENS" surfaces truncated:true on the returned object', async () => {
    const stub = stubFinishReason('MAX_TOKENS');
    try {
        const res = await google.createCall('gemini-2.5-pro', 'k', {})('sys', 'hi', {});
        assert.equal(res.finish_reason, 'length');
        assert.equal(res.truncated, true);
    } finally {
        stub.restore();
    }
});

test('finishReason "STOP" surfaces truncated:false', async () => {
    const stub = stubFinishReason('STOP');
    try {
        const res = await google.createCall('gemini-2.5-pro', 'k', {})('sys', 'hi', {});
        assert.equal(res.finish_reason, 'stop');
        assert.equal(res.truncated, false);
    } finally {
        stub.restore();
    }
});
