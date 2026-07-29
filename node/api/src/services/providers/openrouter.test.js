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

// LLM-560 — billed cost and serving upstream. The catalog here prices the model
// at $10/Mtok so a catalog-derived cost is unmistakably distinct from any billed
// figure the response carries: 10 prompt + 2 completion tokens = 0.00012.
const CATALOG_PRICED = {
    data: [{
        id: 'deepseek/deepseek-v4-flash',
        pricing: { prompt: '0.00001', completion: '0.00001' },
    }],
};

// Stub returning a chat completion with the given usage/provider fields. Pass
// `usage` verbatim so a test can omit `cost` entirely.
function stubCompletion(responseFields) {
    const original = globalThis.fetch;
    // The earlier tests in this file leave an empty catalog cached for 4 hours,
    // so without a reset the priced catalog below would never be fetched and
    // every fallback assertion would silently measure "pricing unknown" instead.
    openrouter._resetCatalogCache();
    globalThis.fetch = async function (url) {
        if (String(url).includes('/models')) return { ok: true, json: async () => CATALOG_PRICED };
        return {
            ok: true,
            json: async () => Object.assign({
                choices: [{ message: { content: 'ok', tool_calls: [] }, finish_reason: 'stop' }],
                usage: { prompt_tokens: 10, completion_tokens: 2 },
            }, responseFields),
        };
    };
    return { restore() { globalThis.fetch = original; } };
}

test('usage.cost comes from the billed figure on the response, not the catalog', async () => {
    const stub = stubCompletion({
        provider: 'Baidu',
        usage: { prompt_tokens: 10, completion_tokens: 2, cost: 0.000001365 },
    });
    try {
        const res = await openrouter.createCall('deepseek/deepseek-v4-flash', 'k', {})('sys', 'hi', {});
        assert.equal(res.usage.cost, 0.000001365);
    } finally {
        stub.restore();
    }
});

test('a response without a billed cost falls back to the catalog estimate', async () => {
    const stub = stubCompletion({ provider: 'Baidu' });
    try {
        const res = await openrouter.createCall('deepseek/deepseek-v4-flash', 'k', {})('sys', 'hi', {});
        // 10 uncached prompt + 2 completion tokens at $10/Mtok
        assert.equal(res.usage.cost, 0.00012);
    } finally {
        stub.restore();
    }
});

test('a malformed billed cost is rejected in favour of the catalog estimate', async () => {
    // NaN, negative, and non-numeric must not reach usage.cost — a bad billed
    // figure would otherwise be trusted over a sane estimate and poison the
    // spend totals, which is the exact class of error this ticket exists to fix.
    for (const bad of ['not-a-number', -1, null]) {
        const stub = stubCompletion({
            provider: 'Baidu',
            usage: { prompt_tokens: 10, completion_tokens: 2, cost: bad },
        });
        try {
            const res = await openrouter.createCall('deepseek/deepseek-v4-flash', 'k', {})('sys', 'hi', {});
            assert.equal(res.usage.cost, 0.00012, 'bad cost ' + JSON.stringify(bad) + ' should fall back');
        } finally {
            stub.restore();
        }
    }
});

test('a zero billed cost is honoured, not treated as missing', async () => {
    // Free-tier and promotional routes legitimately bill 0; falling back to the
    // catalog there would invent spend that never happened.
    const stub = stubCompletion({
        provider: 'Chutes',
        usage: { prompt_tokens: 10, completion_tokens: 2, cost: 0 },
    });
    try {
        const res = await openrouter.createCall('deepseek/deepseek-v4-flash', 'k', {})('sys', 'hi', {});
        assert.equal(res.usage.cost, 0);
    } finally {
        stub.restore();
    }
});

test('the serving upstream is surfaced as usage.served_by', async () => {
    const stub = stubCompletion({
        provider: 'Baidu',
        usage: { prompt_tokens: 10, completion_tokens: 2, cost: 0.000001365 },
    });
    try {
        const res = await openrouter.createCall('deepseek/deepseek-v4-flash', 'k', {})('sys', 'hi', {});
        assert.equal(res.usage.served_by, 'Baidu');
    } finally {
        stub.restore();
    }
});

test('a response with no usable provider name leaves served_by unset', async () => {
    // logCall coalesces a missing served_by to NULL; it must never write a blank.
    // Whitespace-only counts as blank — otherwise it persists as a distinct
    // non-NULL bucket that reads as empty in a spend-by-upstream query.
    for (const bad of [undefined, '', '   ', '\t\n', 42]) {
        const stub = stubCompletion(bad === undefined ? {} : { provider: bad });
        try {
            const res = await openrouter.createCall('deepseek/deepseek-v4-flash', 'k', {})('sys', 'hi', {});
            assert.equal('served_by' in res.usage, false, 'provider ' + JSON.stringify(bad) + ' should not set served_by');
        } finally {
            stub.restore();
        }
    }
});

test('a padded provider name is trimmed, and an overlong one is capped', async () => {
    const stub = stubCompletion({ provider: '  Baidu \n' });
    try {
        const res = await openrouter.createCall('deepseek/deepseek-v4-flash', 'k', {})('sys', 'hi', {});
        assert.equal(res.usage.served_by, 'Baidu');
    } finally {
        stub.restore();
    }

    const longStub = stubCompletion({ provider: 'x'.repeat(500) });
    try {
        const res = await openrouter.createCall('deepseek/deepseek-v4-flash', 'k', {})('sys', 'hi', {});
        assert.equal(res.usage.served_by.length, 100);
    } finally {
        longStub.restore();
    }
});
