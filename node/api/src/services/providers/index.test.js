// Tests for the provider wrapper's turnContext handling (LLM-816): the
// opts boundary (sanitizeOpts), the merge helper (attachTurnContext), and
// the per-provider routing in createProvider — Anthropic gets the field as-is,
// every other provider gets it merged into messages exactly as virtual-agent.js
// merged it before the field existed.
// Run with: node --test (from node/api).

const { test } = require('node:test');
const assert = require('node:assert/strict');
const config = require('../config');
const providers = require('./index');

const { attachTurnContext, sanitizeOpts, createProvider } = providers;

test('attachTurnContext: a latest user turn gets the context appended to its text', () => {
    const messages = [{ role: 'user', content: 'perception' }];
    const out = attachTurnContext(messages, 'ctx');
    assert.deepEqual(out, [{ role: 'user', content: 'perception\n\nctx' }]);
    assert.deepEqual(messages, [{ role: 'user', content: 'perception' }], 'input not mutated');
});

test('attachTurnContext: an empty latest user turn becomes just the context', () => {
    assert.deepEqual(attachTurnContext([{ role: 'user', content: '' }], 'ctx'), [{ role: 'user', content: 'ctx' }]);
});

test('attachTurnContext: after tool results the context is a trailing user turn', () => {
    const messages = [
        { role: 'user', content: 'p' },
        { role: 'assistant', content: '', tool_calls: [{ id: 'c1', type: 'function', function: { name: 'x', arguments: '{}' } }] },
        { role: 'tool', tool_call_id: 'c1', content: 'r' },
    ];
    const out = attachTurnContext(messages, 'ctx');
    assert.equal(out.length, 4);
    assert.deepEqual(out[3], { role: 'user', content: 'ctx' });
    assert.equal(messages.length, 3, 'input not mutated');
});

test('attachTurnContext: no context returns the same messages', () => {
    const messages = [{ role: 'user', content: 'p' }];
    assert.equal(attachTurnContext(messages, ''), messages);
});

test('sanitizeOpts: turnContext passes only with messages', () => {
    const messages = [{ role: 'user', content: 'p' }];
    assert.equal(sanitizeOpts({ messages, turnContext: 'ctx' }).turnContext, 'ctx');
    assert.equal(sanitizeOpts({ cache: true, turnContext: 'ctx' }).turnContext, undefined);
    assert.equal('turnContext' in sanitizeOpts({ messages, turnContext: '' }), false);
});

// Stub fetch and capture the request body for either provider. The OpenRouter
// /models catalog fetch (cost lookup) gets an empty catalog.
function stubFetch() {
    const original = globalThis.fetch;
    let captured = null;
    globalThis.fetch = async function (url, init) {
        if (String(url).includes('/models')) {
            return { ok: true, json: async () => ({ data: [] }) };
        }
        captured = init && init.body ? JSON.parse(init.body) : null;
        if (String(url).includes('anthropic')) {
            return {
                ok: true,
                json: async () => ({
                    content: [{ type: 'text', text: 'ok' }],
                    stop_reason: 'end_turn',
                    usage: { input_tokens: 1, output_tokens: 1 },
                }),
            };
        }
        return {
            ok: true,
            json: async () => ({
                choices: [{ message: { content: 'ok', tool_calls: [] } }],
                usage: { prompt_tokens: 1, completion_tokens: 1 },
            }),
        };
    };
    return {
        restore() { globalThis.fetch = original; },
        lastBody() { return captured; },
    };
}

// The wrapper races each call against this timeout; keep it short so the
// leftover timer does not hold the test process open.
config.set('virtual_agent_request_timeout', '1');

test('createProvider: a provider without acceptsTurnContext gets it merged into messages', async () => {
    const stub = stubFetch();
    try {
        const call = createProvider('openrouter', 'deepseek/deepseek-v4-flash', 'k', {});
        const messages = [{ role: 'user', content: 'perception' }];
        await call('sys', '', { messages, turnContext: 'ctx' });
        const body = stub.lastBody();
        assert.deepEqual(body.messages[body.messages.length - 1], { role: 'user', content: 'perception\n\nctx' });
        assert.equal(body.messages.filter(m => m.role === 'user').length, 1, 'no extra user turn');
    } finally {
        stub.restore();
    }
});

test('createProvider: Anthropic receives turnContext unmerged and places it itself', async () => {
    const stub = stubFetch();
    try {
        const call = createProvider('anthropic', 'claude-opus-5-5', 'k', { cache_prompts: true });
        const messages = [{ role: 'user', content: 'perception' }];
        await call({ static: 's', dynamic: '' }, '', { cache: true, messages, turnContext: 'ctx' });
        assert.deepEqual(stub.lastBody().messages, [{
            role: 'user',
            content: [
                { type: 'text', text: 'perception', cache_control: { type: 'ephemeral' } },
                { type: 'text', text: 'ctx' },
            ],
        }]);
    } finally {
        stub.restore();
    }
});
