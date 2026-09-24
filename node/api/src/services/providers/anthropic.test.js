// Tests for the Anthropic request body across model generations (LLM-672):
// thinking on/off/always-on, temperature, and the registry entries.
// Run with: node --test (from node/api).
//
// createCall issues a real fetch, so globalThis.fetch is stubbed to capture
// the serialized request body and return a minimal Messages response.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const anthropic = require('./anthropic');

function stubFetch() {
    const original = globalThis.fetch;
    let captured = null;
    globalThis.fetch = async function (url, init) {
        captured = init && init.body ? JSON.parse(init.body) : null;
        return {
            ok: true,
            json: async () => ({
                content: [{ type: 'thinking', thinking: '' }, { type: 'text', text: 'ok' }],
                stop_reason: 'end_turn',
                usage: { input_tokens: 10, output_tokens: 2 },
            }),
        };
    };
    return {
        restore() { globalThis.fetch = original; },
        lastBody() { return captured; },
    };
}

async function bodyFor(model, conf) {
    const stub = stubFetch();
    try {
        const call = anthropic.createCall(model, 'k', conf);
        const result = await call('sys', 'hi', {});
        assert.equal(result.text, 'ok', 'thinking blocks are not returned as text');
        return stub.lastBody();
    } finally {
        stub.restore();
    }
}

test('older model, thinking off: no thinking field, temperature passes through', async () => {
    const body = await bodyFor('claude-opus-4-6', { thinking_effort: 'off', temperature: 0.4 });
    assert.equal('thinking' in body, false);
    assert.equal(body.temperature, 0.4);
});

test('older model, thinking on: adaptive + effort, no temperature', async () => {
    const body = await bodyFor('claude-opus-4-6', { thinking_effort: 'high', temperature: 0.4 });
    assert.deepEqual(body.thinking, { type: 'adaptive' });
    assert.deepEqual(body.output_config, { effort: 'high' });
    assert.equal('temperature' in body, false);
});

test('Opus 4.8, thinking off: no thinking field, and temperature is never sent', async () => {
    const body = await bodyFor('claude-opus-4-8', { thinking_effort: 'off', temperature: 0.4 });
    assert.equal('thinking' in body, false);
    assert.equal('temperature' in body, false);
});

for (const model of ['claude-opus-5', 'claude-sonnet-5']) {
    test(model + ', thinking off: sends {type: "disabled"} (omitting it would think)', async () => {
        const body = await bodyFor(model, { thinking_effort: 'off', temperature: 0.4 });
        assert.deepEqual(body.thinking, { type: 'disabled' });
        assert.equal('output_config' in body, false);
        assert.equal('temperature' in body, false);
    });

    test(model + ', thinking on: adaptive + effort, xhigh accepted', async () => {
        const body = await bodyFor(model, { thinking_effort: 'xhigh' });
        assert.deepEqual(body.thinking, { type: 'adaptive' });
        assert.deepEqual(body.output_config, { effort: 'xhigh' });
    });
}

for (const model of ['claude-opus-5-5', 'claude-fable-5-1']) {
    test(model + ', stale "off" becomes low effort, never disabled', async () => {
        const body = await bodyFor(model, { thinking_effort: 'off', temperature: 0.4 });
        assert.deepEqual(body.thinking, { type: 'adaptive' });
        assert.deepEqual(body.output_config, { effort: 'low' });
        assert.equal('temperature' in body, false);
    });

    test(model + ', no effort setting at all still sends a valid body', async () => {
        const body = await bodyFor(model, {});
        assert.deepEqual(body.thinking, { type: 'adaptive' });
        assert.deepEqual(body.output_config, { effort: 'low' });
    });

    test(model + ', chosen effort passes through', async () => {
        const body = await bodyFor(model, { thinking_effort: 'max' });
        assert.deepEqual(body.output_config, { effort: 'max' });
    });

    test(model + ' offers no "off" option in the dashboard', () => {
        const options = anthropic.models[model].capabilities.thinking_effort.options;
        assert.equal(options.includes('off'), false);
        assert.ok(options.includes(anthropic.models[model].capabilities.thinking_effort.default));
    });
}

test('every model entry is complete: API id, pricing, config version, no temperature on rejecting models', () => {
    for (const [key, entry] of Object.entries(anthropic.models)) {
        assert.ok(entry.apiId, key + ' apiId');
        assert.ok(entry.configVersion >= 1, key + ' configVersion');
        for (const field of ['input', 'output', 'cache_write', 'cache_read']) {
            assert.equal(typeof entry.pricing[field], 'number', key + ' pricing.' + field);
        }
        const effort = entry.capabilities.thinking_effort;
        if (effort) {
            assert.ok(effort.options.includes(effort.default), key + ' default effort is a listed option');
        }
    }
    for (const key of ['claude-opus-4-8', 'claude-opus-5', 'claude-opus-5-5', 'claude-sonnet-5', 'claude-fable-5-1', 'claude-opus-4-7']) {
        assert.equal('temperature' in anthropic.models[key].capabilities, false, key + ' must not offer temperature');
    }
});

test('existing models keep their config versions (a bump would mark every saved agent stale)', () => {
    assert.equal(anthropic.models['claude-opus-4-7'].configVersion, 2);
    assert.equal(anthropic.models['claude-opus-4-6'].configVersion, 2);
    assert.equal(anthropic.models['claude-sonnet-4-6'].configVersion, 2);
    assert.equal(anthropic.models['claude-haiku-4-5'].configVersion, 1);
});
