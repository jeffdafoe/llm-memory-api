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

// LLM-930 — Gemini reports thinking tokens apart from the answer
// (usageMetadata.thoughtsTokenCount; totalTokenCount is prompt + tool-use
// prompt + thoughts + candidates) and bills them at the output rate, so they
// belong in output_tokens or the recorded cost is too low.
function stubUsage(usageMetadata) {
    const original = globalThis.fetch;
    globalThis.fetch = async function () {
        return {
            ok: true,
            json: async () => ({
                candidates: [{ content: { parts: [{ text: 'ok' }] }, finishReason: 'STOP' }],
                usageMetadata,
            }),
        };
    };
    return { restore() { globalThis.fetch = original; } };
}

test('thinking tokens count as output tokens', async () => {
    const stub = stubUsage({ promptTokenCount: 10, candidatesTokenCount: 40, thoughtsTokenCount: 900, totalTokenCount: 950 });
    try {
        const res = await google.createCall('gemini-2.5-flash', 'k', {})('sys', 'hi', {});
        assert.equal(res.usage.input_tokens, 10);
        assert.equal(res.usage.output_tokens, 940);
    } finally {
        stub.restore();
    }
});

test('no thoughtsTokenCount (thinking off) leaves output tokens as the candidates count', async () => {
    const stub = stubUsage({ promptTokenCount: 10, candidatesTokenCount: 40, totalTokenCount: 50 });
    try {
        const res = await google.createCall('gemini-2.5-flash', 'k', { thinking_budget: 0 })('sys', 'hi', {});
        assert.equal(res.usage.output_tokens, 40);
    } finally {
        stub.restore();
    }
});

test('an answer cut off while thinking still records the thinking tokens', async () => {
    // Gemini leaves candidatesTokenCount out when no answer text was made.
    const stub = stubUsage({ promptTokenCount: 10, thoughtsTokenCount: 8192, totalTokenCount: 8202 });
    try {
        const res = await google.createCall('gemini-2.5-pro', 'k', {})('sys', 'hi', {});
        assert.equal(res.usage.output_tokens, 8192);
    } finally {
        stub.restore();
    }
});
