// Tests for the /authorize connect page (LLM-729). Run with: node --test
// (from node/api). Uses node:test + node:assert, matching agent.test.js.
//
// The router is mounted on a throwaway Express app on a random port. The db
// pool, actor lookup, API-key lookup and config are stubbed before oauth.js is
// required — oauth.js destructures findApiKeyByToken at require time, so the
// stubs must be in place first. node --test runs each file in its own process,
// so the stubs do not leak into other test files.

const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('crypto');
const express = require('express');

const pool = require('../db');
const actors = require('../services/actors');
const apiKeys = require('../services/api-keys');
const config = require('../services/config');

const UNKNOWN_AGENT = 'nobody';
// resolveByName gets the name but pool.query does not, so the stubbed agent
// row follows the most recent client_id we asked for.
let currentAgent = 'remi';

// Any client_id except UNKNOWN_AGENT resolves to an agent of the same name, so
// a test can choose a hostile name.
actors.resolveByName = async (name) => (name === UNKNOWN_AGENT ? null : { id: 1, name });
pool.query = async () => ({ rows: [{ agent: currentAgent, status: 'active' }] });
apiKeys.findApiKeyByToken = async (secret) => (secret === 'good-secret' ? { id: 1 } : null);
config.get = (key) => (key === 'mcp_oauth_bearer_secret' ? 'test-secret' : undefined);

const oauthRouter = require('./oauth');
const { authCodes } = oauthRouter;

const REDIRECT_URI = 'https://claude.ai/api/mcp/auth_callback';
const VERIFIER = crypto.randomBytes(32).toString('base64url');
const CHALLENGE = crypto.createHash('sha256').update(VERIFIER).digest('base64url');

let server;
let base;

before(async () => {
    const app = express();
    app.use(oauthRouter);
    await new Promise((resolve) => {
        server = app.listen(0, '127.0.0.1', resolve);
    });
    base = `http://127.0.0.1:${server.address().port}`;
});

after(() => {
    server.close();
});

function authorizeParams(overrides = {}) {
    const params = new URLSearchParams({
        response_type: 'code',
        client_id: 'remi',
        redirect_uri: REDIRECT_URI,
        state: 'state-123',
        code_challenge: CHALLENGE,
        code_challenge_method: 'S256'
    });
    for (const [key, value] of Object.entries(overrides)) {
        params.set(key, value);
    }
    return params;
}

async function authorize(params) {
    currentAgent = params.get('client_id');
    return fetch(`${base}/authorize?${params}`, { redirect: 'manual' });
}

// Pull the Continue href out of the page and undo the HTML escaping.
function continueHref(html) {
    const match = html.match(/<a class="btn" href="([^"]+)"/);
    assert.ok(match, 'page has a Continue link');
    return match[1]
        .replace(/&quot;/g, '"')
        .replace(/&#39;/g, "'")
        .replace(/&lt;/g, '<')
        .replace(/&gt;/g, '>')
        .replace(/&amp;/g, '&');
}

test('first pass renders the connect page and mints no code', async () => {
    const codesBefore = authCodes.size;
    const response = await authorize(authorizeParams());
    const html = await response.text();

    assert.equal(response.status, 200);
    assert.match(response.headers.get('content-type'), /text\/html/);
    assert.equal(response.headers.get('cache-control'), 'no-store');
    assert.match(html, /Connecting as remi/);
    assert.match(html, /call its read_instructions tool before you answer/);
    assert.equal(authCodes.size, codesBefore);
});

test('Continue mints exactly one code and redirects with the original state', async () => {
    const first = await authorize(authorizeParams({ state: 'st<"&>' }));
    const href = continueHref(await first.text());

    const codesBefore = authCodes.size;
    const second = await fetch(base + href, { redirect: 'manual' });
    assert.equal(second.status, 302);
    assert.equal(authCodes.size, codesBefore + 1);

    const location = new URL(second.headers.get('location'));
    assert.equal(location.origin + location.pathname, REDIRECT_URI);
    assert.equal(location.searchParams.get('state'), 'st<"&>');
    const code = location.searchParams.get('code');
    assert.ok(authCodes.has(code));

    // The code still exchanges for a token, so the second pass is the old flow.
    const token = await fetch(`${base}/oauth/token`, {
        method: 'POST',
        headers: { 'content-type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({
            grant_type: 'authorization_code',
            code,
            code_verifier: VERIFIER,
            client_id: 'remi',
            client_secret: 'good-secret',
            redirect_uri: REDIRECT_URI
        })
    });
    assert.equal(token.status, 200);
    const body = await token.json();
    assert.ok(body.access_token.startsWith('remi:'));
});

test('Continue is a same-origin /authorize URL that keeps every parameter', async () => {
    const params = authorizeParams({ state: 'a b+c/d?e=f&g' });
    // Repeated parameter, and a confirmed value that must be replaced, not
    // duplicated.
    params.append('resource', 'https://llm-memory.net/mcp');
    params.append('resource', 'https://llm-memory.net/other');
    params.set('confirmed', '0');

    const response = await authorize(params);
    const href = continueHref(await response.text());

    assert.ok(href.startsWith('/authorize?'), href);
    const continued = new URL(href, 'http://placeholder').searchParams;
    assert.deepEqual(continued.getAll('confirmed'), ['1']);
    assert.deepEqual(continued.getAll('resource'), [
        'https://llm-memory.net/mcp',
        'https://llm-memory.net/other'
    ]);
    assert.equal(continued.get('state'), 'a b+c/d?e=f&g');
    assert.equal(continued.get('code_challenge'), CHALLENGE);
    assert.equal(continued.get('redirect_uri'), REDIRECT_URI);
});

test('hostile agent and query values are escaped in the page', async () => {
    const hostile = '<img src=x onerror=alert(1)>';
    const response = await authorize(authorizeParams({
        client_id: hostile,
        state: '"><script>alert(2)</script>'
    }));
    const html = await response.text();

    assert.equal(response.status, 200);
    assert.ok(!html.includes(hostile), 'agent name is not emitted raw');
    assert.ok(html.includes('&lt;img src=x onerror=alert(1)&gt;'));
    assert.ok(!html.includes('<script>alert(2)</script>'), 'state is not emitted raw');
});

test('invalid requests fail before the page, with or without confirmed=1', async () => {
    const cases = [
        { redirect_uri: 'https://evil.example/callback' },
        { client_id: UNKNOWN_AGENT },
        { code_challenge_method: 'plain' },
        { response_type: 'token' }
    ];
    for (const overrides of cases) {
        for (const confirmed of [null, '1']) {
            const params = authorizeParams(overrides);
            if (confirmed) {
                params.set('confirmed', confirmed);
            }
            const codesBefore = authCodes.size;
            const response = await authorize(params);
            const label = JSON.stringify({ ...overrides, confirmed });
            assert.equal(response.status, 400, label);
            assert.match(response.headers.get('content-type'), /application\/json/, label);
            assert.equal(authCodes.size, codesBefore, label);
        }
    }
});
