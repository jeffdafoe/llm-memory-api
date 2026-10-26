// Tests for the gpt-5.6 model registry entries (LLM-340) in the OpenAI
// provider. Run with: node --test (from node/api). The gpt-5.x family is
// generic, so the new variants need no plumbing — these assert the pricing is
// exact (a typo corrupts the budget math, which fails closed on unknown cost)
// and that gpt-5.6 routes through /v1/responses like the rest of the family.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const openai = require('./openai');

test('gpt-5.6 sol/terra/luna are registered with the published pricing', () => {
    assert.deepEqual(openai.models['gpt-5.6-sol'].pricing, { input: 5.00, output: 30, cache_read: 0.50 });
    assert.deepEqual(openai.models['gpt-5.6-terra'].pricing, { input: 2.50, output: 15, cache_read: 0.25 });
    assert.deepEqual(openai.models['gpt-5.6-luna'].pricing, { input: 1.00, output: 6, cache_read: 0.10 });
});

test('gpt-5.6 variants carry the shared gpt-5.x capability shape', () => {
    for (const id of ['gpt-5.6-sol', 'gpt-5.6-terra', 'gpt-5.6-luna']) {
        const model = openai.models[id];
        assert.equal(model.configVersion, 2);
        assert.ok(model.capabilities.reasoning_effort, `${id} has reasoning_effort`);
        assert.deepEqual(model.capabilities.reasoning_effort.options, ['none', 'low', 'medium', 'high', 'xhigh']);
        assert.ok(model.capabilities.service_tier, `${id} has service_tier`);
    }
});

test('gpt-5.6 calls route through /v1/responses like the rest of gpt-5.x', async () => {
    const original = globalThis.fetch;
    let calledUrl = null;
    globalThis.fetch = async function (url) {
        calledUrl = String(url);
        return {
            ok: true,
            json: async () => ({
                output: [{ type: 'message', content: [{ type: 'output_text', text: 'ok' }] }],
                usage: { input_tokens: 10, output_tokens: 2 },
            }),
        };
    };
    try {
        const call = openai.createCall('gpt-5.6-sol', 'k', {});
        const { text } = await call('sys', 'hi', {});
        assert.equal(calledUrl, 'https://api.openai.com/v1/responses');
        assert.equal(text, 'ok');
    } finally {
        globalThis.fetch = original;
    }
});

// LLM-418 — truncation on both endpoints. /v1/responses reports it as
// status "incomplete" + incomplete_details.reason; chat-completions as a
// per-choice finish_reason "length".
test('/v1/responses incomplete/max_output_tokens surfaces truncated:true', async () => {
    const original = globalThis.fetch;
    globalThis.fetch = async function () {
        return {
            ok: true,
            json: async () => ({
                status: 'incomplete',
                incomplete_details: { reason: 'max_output_tokens' },
                output: [{ type: 'message', content: [{ type: 'output_text', text: 'partial' }] }],
                usage: { input_tokens: 10, output_tokens: 8192 },
            }),
        };
    };
    try {
        const res = await openai.createCall('gpt-5.6-sol', 'k', {})('sys', 'hi', {});
        assert.equal(res.finish_reason, 'length');
        assert.equal(res.truncated, true);
    } finally {
        globalThis.fetch = original;
    }
});

test('/v1/responses completed surfaces truncated:false', async () => {
    const original = globalThis.fetch;
    globalThis.fetch = async function () {
        return {
            ok: true,
            json: async () => ({
                status: 'completed',
                output: [{ type: 'message', content: [{ type: 'output_text', text: 'done' }] }],
                usage: { input_tokens: 10, output_tokens: 5 },
            }),
        };
    };
    try {
        const res = await openai.createCall('gpt-5.6-sol', 'k', {})('sys', 'hi', {});
        assert.equal(res.finish_reason, 'stop');
        assert.equal(res.truncated, false);
    } finally {
        globalThis.fetch = original;
    }
});

test('chat-completions finish_reason "length" surfaces truncated:true', async () => {
    const original = globalThis.fetch;
    globalThis.fetch = async function () {
        return {
            ok: true,
            json: async () => ({
                choices: [{ message: { content: 'partial', tool_calls: [] }, finish_reason: 'length' }],
                usage: { prompt_tokens: 10, completion_tokens: 4096 },
            }),
        };
    };
    try {
        // gpt-4o-mini stays on /v1/chat/completions (not gpt-5.x, not o-series).
        const res = await openai.createCall('gpt-4o-mini', 'k', {})('sys', 'hi', {});
        assert.equal(res.finish_reason, 'length');
        assert.equal(res.truncated, true);
    } finally {
        globalThis.fetch = original;
    }
});

// LLM-871 — gpt-6.x entries. Pricing must be exact (cost tracking), calls
// must use /v1/responses (tool calling needs it), and reasoning "none" must
// be sent explicitly because the gpt-6.x default is "medium".

// Captures the request body of one createCall round-trip.
async function captureResponsesBody(modelId, conf, usage) {
    const original = globalThis.fetch;
    const captured = {};
    globalThis.fetch = async function (url, init) {
        captured.url = String(url);
        captured.body = JSON.parse(init.body);
        return {
            ok: true,
            json: async () => ({
                status: 'completed',
                output: [{ type: 'message', content: [{ type: 'output_text', text: 'ok' }] }],
                usage: usage || { input_tokens: 10, output_tokens: 2 },
            }),
        };
    };
    try {
        captured.result = await openai.createCall(modelId, 'k', conf)('sys', 'hi', {});
    } finally {
        globalThis.fetch = original;
    }
    return captured;
}

test('gpt-6.1-sol, gpt-6.1-terra and gpt-6-luna are registered with the published pricing', () => {
    assert.deepEqual(openai.models['gpt-6.1-sol'].pricing, { input: 2.00, output: 10, cache_read: 0.10 });
    assert.deepEqual(openai.models['gpt-6.1-terra'].pricing, { input: 1.00, output: 5, cache_read: 0.05 });
    assert.deepEqual(openai.models['gpt-6-luna'].pricing, { input: 0.10, output: 0.50, cache_read: 0.01 });
    assert.equal(openai.models['gpt-6.1-luna'], undefined);
});

test('gpt-6.x capability shape: medium default, per-model reasoning options, no temperature', () => {
    const all = ['none', 'low', 'medium', 'high', 'xhigh', 'max'];
    const expected = {
        'gpt-6.1-sol': ['low', 'medium', 'high', 'xhigh', 'max'],
        'gpt-6.1-terra': all,
        'gpt-6-luna': all,
    };
    for (const [id, options] of Object.entries(expected)) {
        const model = openai.models[id];
        assert.equal(model.configVersion, 3, id);
        assert.equal(model.capabilities.reasoning_effort.default, 'medium', id);
        assert.deepEqual(model.capabilities.reasoning_effort.options, options, id);
        assert.equal(model.capabilities.temperature, undefined, id);
        assert.ok(model.capabilities.service_tier, id);
    }
    // Per-model clones: changing one model's options must not leak to another.
    openai.models['gpt-6.1-terra'].capabilities.reasoning_effort.options.push('leak');
    assert.deepEqual(openai.models['gpt-6-luna'].capabilities.reasoning_effort.options, all);
    openai.models['gpt-6.1-terra'].capabilities.reasoning_effort.options.pop();
});

test('gpt-6.x calls route through /v1/responses', async () => {
    for (const id of ['gpt-6.1-sol', 'gpt-6.1-terra', 'gpt-6-luna']) {
        const { url, result } = await captureResponsesBody(id, {});
        assert.equal(url, 'https://api.openai.com/v1/responses', id);
        assert.equal(result.text, 'ok', id);
    }
});

test('gpt-6.x sends reasoning effort "none" explicitly; gpt-5.x still omits it', async () => {
    const gpt6 = await captureResponsesBody('gpt-6-luna', { reasoning_effort: 'none' });
    assert.deepEqual(gpt6.body.reasoning, { effort: 'none' });

    const gpt6Max = await captureResponsesBody('gpt-6.1-sol', { reasoning_effort: 'max' });
    assert.deepEqual(gpt6Max.body.reasoning, { effort: 'max' });

    // Unset leaves OpenAI's default (medium) in charge.
    const gpt6Unset = await captureResponsesBody('gpt-6.1-terra', {});
    assert.equal(gpt6Unset.body.reasoning, undefined);

    const gpt5 = await captureResponsesBody('gpt-5.6-luna', { reasoning_effort: 'none' });
    assert.equal(gpt5.body.reasoning, undefined);
});

test('gpt-6.x cost: standard rates at or below 272K input', async () => {
    // 272,000 input (200,000 cached) + 10,000 output on gpt-6.1-sol:
    // 72,000 * 2.00 + 200,000 * 0.10 + 10,000 * 10, all per 1M.
    const { result } = await captureResponsesBody('gpt-6.1-sol', {}, {
        input_tokens: 272000,
        input_tokens_details: { cached_tokens: 200000 },
        output_tokens: 10000,
    });
    const expected = (72000 * 2.00 + 200000 * 0.10 + 10000 * 10) / 1_000_000;
    assert.ok(Math.abs(result.usage.cost - expected) < 1e-12, `cost ${result.usage.cost} != ${expected}`);
});

test('gpt-6.x cost: long-context surcharge above 272K applies to the whole request', async () => {
    // 300,000 input (100,000 cached) + 10,000 output on gpt-6.1-terra:
    // input and cache 2x, output 1.5x.
    const { result } = await captureResponsesBody('gpt-6.1-terra', {}, {
        input_tokens: 300000,
        input_tokens_details: { cached_tokens: 100000 },
        output_tokens: 10000,
    });
    const expected = (200000 * 1.00 * 2 + 100000 * 0.05 * 2 + 10000 * 5 * 1.5) / 1_000_000;
    assert.ok(Math.abs(result.usage.cost - expected) < 1e-12, `cost ${result.usage.cost} != ${expected}`);
});

test('gpt-5.x cost is unchanged above 272K input (no long-context entry)', async () => {
    const { result } = await captureResponsesBody('gpt-5.6-luna', {}, {
        input_tokens: 300000,
        output_tokens: 10000,
    });
    const expected = (300000 * 1.00 + 10000 * 6) / 1_000_000;
    assert.ok(Math.abs(result.usage.cost - expected) < 1e-12, `cost ${result.usage.cost} != ${expected}`);
});
