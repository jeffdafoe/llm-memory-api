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

    test(model + ', no effort setting: thinking omitted so the model default (adaptive) applies', async () => {
        const body = await bodyFor(model, {});
        assert.equal('thinking' in body, false, 'no setting must not disable thinking');
        assert.equal('output_config' in body, false);
    });

    test(model + ', thinking on: adaptive + effort, xhigh accepted', async () => {
        const body = await bodyFor(model, { thinking_effort: 'xhigh' });
        assert.deepEqual(body.thinking, { type: 'adaptive' });
        assert.deepEqual(body.output_config, { effort: 'xhigh' });
    });
}

for (const model of ['claude-opus-5-5', 'claude-fable-5-1', 'claude-haiku-5-5']) {
    test(model + ', stale "off" becomes low effort, never disabled', async () => {
        const body = await bodyFor(model, { thinking_effort: 'off', temperature: 0.4 });
        assert.deepEqual(body.thinking, { type: 'adaptive' });
        assert.deepEqual(body.output_config, { effort: 'low' });
        assert.equal('temperature' in body, false);
    });

    test(model + ', no effort setting: no effort sent, so Anthropic\'s default for the model applies', async () => {
        const body = await bodyFor(model, {});
        assert.deepEqual(body.thinking, { type: 'adaptive' });
        assert.equal('output_config' in body, false, 'absent must not be treated as a stale "off"');
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
    for (const key of ['claude-opus-4-8', 'claude-opus-5', 'claude-opus-5-5', 'claude-sonnet-5', 'claude-fable-5-1', 'claude-opus-4-7', 'claude-haiku-5-5']) {
        assert.equal('temperature' in anthropic.models[key].capabilities, false, key + ' must not offer temperature');
    }
});

test('existing models keep their config versions (a bump would mark every saved agent stale)', () => {
    assert.equal(anthropic.models['claude-opus-4-7'].configVersion, 2);
    assert.equal(anthropic.models['claude-opus-4-6'].configVersion, 2);
    assert.equal(anthropic.models['claude-sonnet-4-6'].configVersion, 2);
    assert.equal(anthropic.models['claude-haiku-4-5'].configVersion, 1);
});

// ── Conversation cache breakpoint (LLM-816) ─────────────────────────────────

const CACHED_CONF = { cache_prompts: true };
const SYSTEM = { static: 'static rules', dynamic: '' };

async function bodyForCall(conf, opts) {
    const stub = stubFetch();
    try {
        const call = anthropic.createCall('claude-opus-5-5', 'k', conf);
        await call(SYSTEM, '', opts);
        return stub.lastBody();
    } finally {
        stub.restore();
    }
}

// A court-style tool loop in the neutral (OpenAI) shape: opener, one round of
// a tool call and its result.
function historyAfterRounds(rounds) {
    const messages = [{ role: 'user', content: 'The court is in session.' }];
    for (let i = 1; i <= rounds; i++) {
        messages.push({
            role: 'assistant',
            content: 'Reading record ' + i + '.',
            tool_calls: [{ id: 'call_' + i, type: 'function', function: { name: 'read_record', arguments: '{"n":' + i + '}' } }],
        });
        messages.push({ role: 'tool', tool_call_id: 'call_' + i, content: 'record ' + i + ' text' });
    }
    return messages;
}

// Every cache_control location in the request messages, as "msgIndex:blockIndex".
function markerPositions(body) {
    const found = [];
    body.messages.forEach((msg, i) => {
        if (Array.isArray(msg.content)) {
            msg.content.forEach((block, j) => {
                if (block.cache_control) found.push(i + ':' + j);
            });
        }
    });
    return found;
}

test('tool loop with caching on: marker on the last tool_result, system breakpoint kept, no top-level marker', async () => {
    const body = await bodyForCall(CACHED_CONF, { cache: true, messages: historyAfterRounds(2) });
    const last = body.messages[body.messages.length - 1];
    assert.equal(last.role, 'user');
    assert.equal(last.content[last.content.length - 1].type, 'tool_result');
    assert.deepEqual(markerPositions(body), [(body.messages.length - 1) + ':' + (last.content.length - 1)]);
    assert.deepEqual(body.system[0].cache_control, { type: 'ephemeral' });
    assert.equal('cache_control' in body, false);
});

test('fresh perception + turnContext: marker on the perception block, context after it unmarked', async () => {
    const messages = historyAfterRounds(1).concat([{ role: 'user', content: 'A new perception.' }]);
    const body = await bodyForCall(CACHED_CONF, { cache: true, messages, turnContext: 'volatile now' });
    const last = body.messages[body.messages.length - 1];
    assert.deepEqual(last.content, [
        { type: 'text', text: 'A new perception.', cache_control: { type: 'ephemeral' } },
        { type: 'text', text: 'volatile now' },
    ]);
});

test('tool results + turnContext: context is a text block after the marked tool_result', async () => {
    const body = await bodyForCall(CACHED_CONF, { cache: true, messages: historyAfterRounds(1), turnContext: 'volatile now' });
    const last = body.messages[body.messages.length - 1];
    assert.equal(last.content.length, 2);
    assert.equal(last.content[0].type, 'tool_result');
    assert.deepEqual(last.content[0].cache_control, { type: 'ephemeral' });
    assert.deepEqual(last.content[1], { type: 'text', text: 'volatile now' });
});

test('caching off: no marker in messages, turnContext still a separate block', async () => {
    const messages = [{ role: 'user', content: 'A perception.' }];
    const body = await bodyForCall({}, { cache: true, messages, turnContext: 'volatile now' });
    assert.deepEqual(markerPositions(body), []);
    assert.deepEqual(body.messages[0].content, [
        { type: 'text', text: 'A perception.' },
        { type: 'text', text: 'volatile now' },
    ]);
});

test('single-message call with caching on: no marker on the message, system breakpoint kept', async () => {
    const stub = stubFetch();
    try {
        const call = anthropic.createCall('claude-opus-5-5', 'k', CACHED_CONF);
        await call(SYSTEM, 'one-off question', { cache: true });
        const body = stub.lastBody();
        assert.deepEqual(body.messages, [{ role: 'user', content: 'one-off question' }]);
        assert.deepEqual(body.system[0].cache_control, { type: 'ephemeral' });
    } finally {
        stub.restore();
    }
});

test('caching on and no opts at all: the call still goes out, with only the system breakpoint', async () => {
    const stub = stubFetch();
    try {
        const call = anthropic.createCall('claude-opus-5-5', 'k', CACHED_CONF);
        const result = await call(SYSTEM, 'one-off question');
        assert.equal(result.text, 'ok');
        assert.deepEqual(stub.lastBody().messages, [{ role: 'user', content: 'one-off question' }]);
    } finally {
        stub.restore();
    }
});

test('an empty last text is never marked (Anthropic rejects cache_control on an empty text block)', async () => {
    const body = await bodyForCall(CACHED_CONF, { cache: true, messages: [{ role: 'user', content: '' }] });
    assert.deepEqual(markerPositions(body), []);
});

// The property the cache depends on: what round N sent (minus its turn
// context) is a prefix of what round N+1 sends. Markers are stripped and
// string content is normalized to its documented one-text-block form first.
function normalize(messages) {
    return messages.map((msg) => {
        let content = msg.content;
        if (typeof content === 'string') {
            content = [{ type: 'text', text: content }];
        }
        content = content.map((block) => {
            const copy = { ...block };
            delete copy.cache_control;
            return copy;
        });
        return { role: msg.role, content };
    });
}

test('round N history is a prefix of round N+1 (the cached entry is reachable)', async () => {
    const roundN = await bodyForCall(CACHED_CONF, { cache: true, messages: historyAfterRounds(2), turnContext: 'ctx N' });
    const roundNext = await bodyForCall(CACHED_CONF, { cache: true, messages: historyAfterRounds(3), turnContext: 'ctx N+1' });
    const prev = normalize(roundN.messages);
    // Drop the turn context block from round N's last message: it is never replayed.
    prev[prev.length - 1].content = prev[prev.length - 1].content.filter(b => b.text !== 'ctx N');
    const next = normalize(roundNext.messages);
    assert.deepEqual(next.slice(0, prev.length), prev);
});
