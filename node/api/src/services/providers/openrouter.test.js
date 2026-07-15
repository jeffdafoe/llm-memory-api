// Tests for the OpenRouter provider-routing passthrough (LLM-328) in
// createCall. Run with: node --test (from node/api). Uses the built-in
// node:test runner + node:assert, matching the other *.test.js modules.
//
// The call function issues a real fetch, so we stub globalThis.fetch to
// capture the serialized request body without hitting the network. The stub
// branches on URL: the /models catalog fetch (triggered by computeCost →
// lookupPricing) gets an empty catalog; the chat/completions call gets a
// minimal OpenAI-shape response and its body is captured for assertions.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const openrouter = require('./openrouter');

// Install a fetch stub that records the chat-completions request body and
// returns canned responses. Returns { restore, lastBody() }.
function stubFetch() {
    const original = globalThis.fetch;
    let captured = null;
    globalThis.fetch = async function (url, init) {
        if (String(url).includes('/models')) {
            // Catalog fetch — return an empty catalog so computeCost resolves
            // to null (pricing unknown) without a network call.
            return { ok: true, json: async () => ({ data: [] }) };
        }
        captured = init && init.body ? JSON.parse(init.body) : null;
        return {
            ok: true,
            json: async () => ({
                choices: [{ message: { content: 'ok', tool_calls: [] } }],
                usage: { prompt_tokens: 10, completion_tokens: 2 },
            }),
        };
    };
    return {
        restore() { globalThis.fetch = original; },
        lastBody() { return captured; },
    };
}

test('conf.provider reaches body.provider with the same JSON shape on the wire', async () => {
    const stub = stubFetch();
    try {
        const routing = { order: ['deepinfra'], allow_fallbacks: false };
        const call = openrouter.createCall('deepseek/deepseek-v4-flash', 'k', { provider: routing });
        await call('sys', 'hi', {});
        // lastBody() is the JSON-round-tripped request body, so this asserts the
        // serialized wire shape (what OpenRouter sees), not object identity.
        assert.deepEqual(stub.lastBody().provider, routing);
    } finally {
        stub.restore();
    }
});

test('no conf.provider leaves body.provider unset', async () => {
    const stub = stubFetch();
    try {
        const call = openrouter.createCall('deepseek/deepseek-v4-flash', 'k', {});
        await call('sys', 'hi', {});
        assert.equal('provider' in stub.lastBody(), false);
    } finally {
        stub.restore();
    }
});

test('a non-object conf.provider is ignored (guard against malformed config)', async () => {
    const stub = stubFetch();
    try {
        // scalar and array are both rejected — only a plain routing object ships
        for (const bad of ['deepinfra', ['deepinfra']]) {
            const call = openrouter.createCall('deepseek/deepseek-v4-flash', 'k', { provider: bad });
            await call('sys', 'hi', {});
            assert.equal('provider' in stub.lastBody(), false);
        }
    } finally {
        stub.restore();
    }
});

// LLM-418 — the truncation signal. OpenRouter fronts the affected Gemini soul
// agent, so a "length" finish_reason here is the live truncation path.
function stubFinishReason(finish) {
    const original = globalThis.fetch;
    globalThis.fetch = async function (url) {
        if (String(url).includes('/models')) return { ok: true, json: async () => ({ data: [] }) };
        return {
            ok: true,
            json: async () => ({
                choices: [{ message: { content: 'partial', tool_calls: [] }, finish_reason: finish }],
                usage: { prompt_tokens: 10, completion_tokens: 4096 },
            }),
        };
    };
    return { restore() { globalThis.fetch = original; } };
}

test('finish_reason "length" surfaces truncated:true on the returned object', async () => {
    const stub = stubFinishReason('length');
    try {
        const res = await openrouter.createCall('google/gemini-2.5-pro', 'k', {})('sys', 'hi', {});
        assert.equal(res.finish_reason, 'length');
        assert.equal(res.truncated, true);
    } finally {
        stub.restore();
    }
});

test('finish_reason "stop" surfaces truncated:false', async () => {
    const stub = stubFinishReason('stop');
    try {
        const res = await openrouter.createCall('google/gemini-2.5-pro', 'k', {})('sys', 'hi', {});
        assert.equal(res.finish_reason, 'stop');
        assert.equal(res.truncated, false);
    } finally {
        stub.restore();
    }
});
