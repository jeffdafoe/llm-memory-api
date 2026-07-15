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
