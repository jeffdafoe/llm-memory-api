// Which URL each outside call goes to (LLM-906): today's URL when no
// *_API_BASE variable is set, and <base><path> when one is.
// Run with: node --test (from node/api).
//
// globalThis.fetch is stubbed to record every URL. It answers a non-OK
// status, so each call fails after its fetch and the test only looks at the
// URLs. The one exception is OpenRouter's chat call, which gets a valid reply
// so the provider goes on to fetch its model catalog (computeCost); the
// catalog fetch gets a non-OK answer, which the provider does not cache, so
// every test sees its own catalog fetch.

const { test, afterEach } = require('node:test');
const assert = require('node:assert/strict');
const { DEFAULTS } = require('../api-base');
const anthropic = require('./anthropic');
const openai = require('./openai');
const google = require('./google');
const openrouter = require('./openrouter');
const perplexity = require('./perplexity');
const xai = require('./xai');
const { embed } = require('../embeddings');

const saved = {};
for (const name of Object.keys(DEFAULTS)) {
    saved[name] = process.env[name];
}
const originalFetch = globalThis.fetch;

afterEach(() => {
    globalThis.fetch = originalFetch;
    for (const name of Object.keys(saved)) {
        if (saved[name] === undefined) {
            delete process.env[name];
        } else {
            process.env[name] = saved[name];
        }
    }
});

function stubFetch() {
    const urls = [];
    globalThis.fetch = async function (url) {
        urls.push(String(url));
        if (String(url).endsWith('/api/v1/chat/completions')) {
            return {
                ok: true,
                json: async () => ({
                    choices: [{ message: { content: 'ok', tool_calls: [] } }],
                    usage: { prompt_tokens: 10, completion_tokens: 2 }
                })
            };
        }
        return { ok: false, status: 418, text: async () => 'stub', json: async () => ({}) };
    };
    return urls;
}

// Runs fn with all *_API_BASE variables cleared, then with `name` set to
// `base`, and returns the URLs fetched each time.
async function urlsFor(name, base, fn) {
    for (const key of Object.keys(DEFAULTS)) {
        delete process.env[key];
    }
    const result = {};
    for (const mode of ['default', 'override']) {
        if (mode === 'override') {
            process.env[name] = base;
        }
        const urls = stubFetch();
        try {
            await fn();
        } catch (err) {
            // Expected: the stub answers 418.
        }
        result[mode] = urls;
    }
    return result;
}

const BASE = 'http://127.0.0.1:9100/fake/';

test('Anthropic messages', async () => {
    const urls = await urlsFor('ANTHROPIC_API_BASE', BASE, () =>
        anthropic.createCall('claude-opus-5-5', 'k', {})('sys', 'hi', {}));
    assert.deepEqual(urls.default, ['https://api.anthropic.com/v1/messages']);
    assert.deepEqual(urls.override, ['http://127.0.0.1:9100/fake/v1/messages']);
});

test('OpenAI responses (gpt-5.x)', async () => {
    const urls = await urlsFor('OPENAI_API_BASE', BASE, () =>
        openai.createCall('gpt-5.5', 'k', {})('sys', 'hi', {}));
    assert.deepEqual(urls.default, ['https://api.openai.com/v1/responses']);
    assert.deepEqual(urls.override, ['http://127.0.0.1:9100/fake/v1/responses']);
});

test('OpenAI chat completions (older models)', async () => {
    const urls = await urlsFor('OPENAI_API_BASE', BASE, () =>
        openai.createCall('gpt-4.1', 'k', {})('sys', 'hi', {}));
    assert.deepEqual(urls.default, ['https://api.openai.com/v1/chat/completions']);
    assert.deepEqual(urls.override, ['http://127.0.0.1:9100/fake/v1/chat/completions']);
});

test('OpenAI embeddings', async () => {
    const urls = await urlsFor('OPENAI_API_BASE', BASE, () => embed('hi'));
    assert.deepEqual(urls.default, ['https://api.openai.com/v1/embeddings']);
    assert.deepEqual(urls.override, ['http://127.0.0.1:9100/fake/v1/embeddings']);
});

test('Google generateContent', async () => {
    const urls = await urlsFor('GOOGLE_API_BASE', BASE, () =>
        google.createCall('gemini-2.5-flash', 'k', {})('sys', 'hi', {}));
    assert.deepEqual(urls.default, ['https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent']);
    assert.deepEqual(urls.override, ['http://127.0.0.1:9100/fake/v1beta/models/gemini-2.5-flash:generateContent']);
});

test('OpenRouter chat completions and model catalog', async () => {
    const urls = await urlsFor('OPENROUTER_API_BASE', BASE, () =>
        openrouter.createCall('deepseek/deepseek-v4-flash', 'k', {})('sys', 'hi', {}));
    assert.deepEqual(urls.default, [
        'https://openrouter.ai/api/v1/chat/completions',
        'https://openrouter.ai/api/v1/models'
    ]);
    assert.deepEqual(urls.override, [
        'http://127.0.0.1:9100/fake/api/v1/chat/completions',
        'http://127.0.0.1:9100/fake/api/v1/models'
    ]);
});

test('Perplexity chat completions', async () => {
    const urls = await urlsFor('PERPLEXITY_API_BASE', BASE, () =>
        perplexity.createCall('sonar', 'k', {})('sys', 'hi', {}));
    assert.deepEqual(urls.default, ['https://api.perplexity.ai/chat/completions']);
    assert.deepEqual(urls.override, ['http://127.0.0.1:9100/fake/chat/completions']);
});

test('xAI responses', async () => {
    const urls = await urlsFor('XAI_API_BASE', BASE, () =>
        xai.createCall('grok-4.20-reasoning', 'k', {})('sys', 'hi', {}));
    assert.deepEqual(urls.default, ['https://api.x.ai/v1/responses']);
    assert.deepEqual(urls.override, ['http://127.0.0.1:9100/fake/v1/responses']);
});

test('one upstream\'s variable does not move another upstream', async () => {
    const urls = await urlsFor('XAI_API_BASE', BASE, () =>
        openai.createCall('gpt-5.5', 'k', {})('sys', 'hi', {}));
    assert.deepEqual(urls.override, ['https://api.openai.com/v1/responses']);
});
