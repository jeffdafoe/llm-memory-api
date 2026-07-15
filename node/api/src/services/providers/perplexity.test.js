// Tests for Perplexity source extraction (LLM-340) in createCall. Run with:
// node --test (from node/api). Uses the built-in node:test runner +
// node:assert, matching the other *.test.js provider modules.
//
// Perplexity removed the top-level `citations` array (plain URL strings) in
// May 2025 in favor of `search_results` ({ title, url, date }). These tests
// stub globalThis.fetch to return canned response shapes and assert the
// "Sources:" block the provider appends to the model text.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const perplexity = require('./perplexity');

// Stub fetch to return a fixed Perplexity chat-completions payload. `extra`
// is merged onto the response body so each test supplies its own
// search_results / citations fields.
function stubFetch(extra) {
    const original = globalThis.fetch;
    globalThis.fetch = async function () {
        return {
            ok: true,
            json: async () => Object.assign({
                choices: [{ message: { content: 'answer body' } }],
                usage: { prompt_tokens: 10, completion_tokens: 5 },
            }, extra),
        };
    };
    return { restore() { globalThis.fetch = original; } };
}

async function callSonar(extra, conf) {
    const stub = stubFetch(extra);
    try {
        const call = perplexity.createCall('sonar', 'k', conf || {});
        return await call('sys', 'q', {});
    } finally {
        stub.restore();
    }
}

test('search_results renders a Sources block with title, url and date', async () => {
    const { text } = await callSonar({
        search_results: [
            { title: 'First Source', url: 'https://a.example/1', date: '2026-07-01' },
            { title: 'Second Source', url: 'https://b.example/2', date: '2026-07-02' },
        ],
    });
    assert.match(text, /\n\nSources:\n/);
    assert.match(text, /\[1\] First Source — https:\/\/a\.example\/1 \(2026-07-01\)/);
    assert.match(text, /\[2\] Second Source — https:\/\/b\.example\/2 \(2026-07-02\)/);
});

test('search_results entries missing title/date degrade to just the url', async () => {
    const { text } = await callSonar({
        search_results: [{ url: 'https://a.example/1' }],
    });
    assert.match(text, /\[1\] https:\/\/a\.example\/1/);
    // no " — " title separator and no empty "()" date
    assert.doesNotMatch(text, / — /);
    assert.doesNotMatch(text, /\(\)/);
});

test('legacy citations array is used when search_results is absent', async () => {
    const { text } = await callSonar({
        citations: ['https://a.example/1', 'https://b.example/2'],
    });
    assert.match(text, /\[1\] https:\/\/a\.example\/1/);
    assert.match(text, /\[2\] https:\/\/b\.example\/2/);
});

test('search_results takes precedence over legacy citations', async () => {
    const { text } = await callSonar({
        search_results: [{ title: 'New', url: 'https://new.example', date: '2026-07-03' }],
        citations: ['https://old.example'],
    });
    assert.match(text, /https:\/\/new\.example/);
    assert.doesNotMatch(text, /old\.example/);
});

test('no search_results or citations means no Sources block', async () => {
    const { text } = await callSonar({});
    assert.equal(text, 'answer body');
});

test('return_citations=false suppresses the Sources block', async () => {
    const { text } = await callSonar(
        { search_results: [{ title: 'X', url: 'https://x.example', date: '2026-07-01' }] },
        { return_citations: false },
    );
    assert.equal(text, 'answer body');
});

// LLM-418 — finish_reason lives on choices[0], so the shared stubFetch's
// top-level `extra` merge won't reach it; use a dedicated stub.
test('finish_reason "length" surfaces truncated:true', async () => {
    const original = globalThis.fetch;
    globalThis.fetch = async function () {
        return {
            ok: true,
            json: async () => ({
                choices: [{ message: { content: 'partial' }, finish_reason: 'length' }],
                usage: { prompt_tokens: 10, completion_tokens: 4096 },
            }),
        };
    };
    try {
        const res = await perplexity.createCall('sonar', 'k', {})('sys', 'q', {});
        assert.equal(res.finish_reason, 'length');
        assert.equal(res.truncated, true);
    } finally {
        globalThis.fetch = original;
    }
});
