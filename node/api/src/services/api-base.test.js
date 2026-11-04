// Tests for the outside-API base URLs (LLM-906).
// Run with: node --test (from node/api).

const { test, afterEach } = require('node:test');
const assert = require('node:assert/strict');
const { apiBase, checkApiBases, DEFAULTS } = require('./api-base');

const NAMES = Object.keys(DEFAULTS);
const saved = {};
for (const name of NAMES) {
    saved[name] = process.env[name];
}

afterEach(() => {
    for (const name of NAMES) {
        if (saved[name] === undefined) {
            delete process.env[name];
        } else {
            process.env[name] = saved[name];
        }
    }
});

test('unset or blank gives the default origin', () => {
    delete process.env.OPENAI_API_BASE;
    assert.equal(apiBase('OPENAI_API_BASE'), 'https://api.openai.com');
    process.env.OPENAI_API_BASE = '   ';
    assert.equal(apiBase('OPENAI_API_BASE'), 'https://api.openai.com');
});

test('every default is an https origin with no trailing slash', () => {
    for (const name of NAMES) {
        const url = new URL(DEFAULTS[name]);
        assert.equal(url.protocol, 'https:', name);
        assert.equal(DEFAULTS[name], url.origin, name);
    }
});

test('an override is used, with trailing slashes dropped', () => {
    process.env.ANTHROPIC_API_BASE = 'http://127.0.0.1:9100/';
    assert.equal(apiBase('ANTHROPIC_API_BASE'), 'http://127.0.0.1:9100');
    process.env.ANTHROPIC_API_BASE = ' http://127.0.0.1:9100// ';
    assert.equal(apiBase('ANTHROPIC_API_BASE'), 'http://127.0.0.1:9100');
});

test('a path prefix is kept', () => {
    process.env.XAI_API_BASE = 'http://127.0.0.1:9100/xai/';
    assert.equal(apiBase('XAI_API_BASE'), 'http://127.0.0.1:9100/xai');
});

test('the value is read on each call', () => {
    process.env.GOOGLE_API_BASE = 'http://a.test';
    assert.equal(apiBase('GOOGLE_API_BASE'), 'http://a.test');
    process.env.GOOGLE_API_BASE = 'http://b.test';
    assert.equal(apiBase('GOOGLE_API_BASE'), 'http://b.test');
});

test('an invalid value throws an error naming the variable', () => {
    const bad = [
        'not a url',
        'ftp://example.test',
        'http://example.test/?x=1',
        'http://example.test/#frag',
        'http://example.test/?',
        'http://example.test/#',
        'http://user:pass@example.test'
    ];
    for (const value of bad) {
        process.env.PERPLEXITY_API_BASE = value;
        assert.throws(() => apiBase('PERPLEXITY_API_BASE'), /PERPLEXITY_API_BASE/, value);
    }
});

test('an error never repeats the value, which may hold a secret', () => {
    const secrets = [
        'https://user:s3cret-pw@example.test',
        'https://example.test?api_key=s3cret-pw',
        'ftp://s3cret-pw.example.test',
        's3cret-pw not a url'
    ];
    for (const value of secrets) {
        process.env.XAI_API_BASE = value;
        assert.throws(() => apiBase('XAI_API_BASE'), (err) => {
            assert.match(err.message, /XAI_API_BASE/);
            assert.doesNotMatch(err.message, /s3cret-pw/, value);
            return true;
        });
    }
});

test('an unknown variable name throws', () => {
    assert.throws(() => apiBase('NOPE_API_BASE'), /Unknown API base variable/);
});

test('checkApiBases passes with nothing set and throws on one bad value', () => {
    for (const name of NAMES) {
        delete process.env[name];
    }
    checkApiBases();
    process.env.OPENROUTER_API_BASE = 'mailto:x@example.test';
    assert.throws(() => checkApiBases(), /OPENROUTER_API_BASE/);
});
