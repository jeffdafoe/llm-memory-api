// Tests for the claude.ai connector added with only the URL (LLM-733): OAuth
// metadata, dynamic client registration, CIMD client_ids, the login on
// /authorize, and the secret-less token exchange. Run with: node --test (from
// node/api). Uses node:test + node:assert, matching oauth.test.js.
//
// The router is mounted on a throwaway Express app on a random port, with
// express.json app-wide as server.js has it (DCR posts JSON). The db pool,
// actor lookup, API-key lookup and config are stubbed before oauth.js is
// required. Passwords are hashed for real, so the login runs the same check
// as the dashboard login. node --test runs each file in its own process, so
// these stubs and the login rate limiters do not leak into other test files.

const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('crypto');
const express = require('express');

const pool = require('../db');
const actors = require('../services/actors');
const apiKeys = require('../services/api-keys');
const config = require('../services/config');
const { hash, generateSalt } = require('../services/hashing');

const BEARER_SECRET = 'test-secret';
const PASSWORD = 'correct horse battery';
const CLAUDE_CALLBACK = 'https://claude.ai/api/mcp/auth_callback';
const CLAUDE_COM_CALLBACK = 'https://claude.com/api/mcp/auth_callback';
const CIMD_CLIENT_ID = 'https://claude.ai/oauth/mcp-oauth-client-metadata';

// Agents that have a password; filled in before() once the hashes exist.
const passwordRows = {};

config.get = (key) => (key === 'mcp_oauth_bearer_secret' ? BEARER_SECRET : undefined);
apiKeys.findApiKeyByToken = async (secret) => (secret === 'good-secret' ? { id: 1 } : null);
// Agent-name client_ids (the older path): only these two exist.
actors.resolveByName = async (name) => (['remi', 'alice'].includes(name) ? { id: 1, name } : null);
pool.query = async (sql, params) => {
    if (/password_hash IS NOT NULL/.test(sql)) {
        const row = passwordRows[params[0]];
        return { rows: row ? [row] : [] };
    }
    // The older path's "agent exists" read: the last name resolved.
    return { rows: [{ agent: 'remi', status: 'active' }] };
};

const oauthRouter = require('./oauth');
const { authCodes } = oauthRouter;

let server;
let base;

before(async () => {
    for (const [id, name] of [[7, 'alice'], [8, 'bob'], [9, 'carol']]) {
        const salt = generateSalt();
        passwordRows[name] = { id, name, password_salt: salt, password_hash: await hash(PASSWORD, salt) };
    }
    const app = express();
    app.use(express.json());
    app.use(oauthRouter);
    await new Promise((resolve) => {
        server = app.listen(0, '127.0.0.1', resolve);
    });
    base = `http://127.0.0.1:${server.address().port}`;
});

after(() => {
    server.close();
});

function pkce() {
    const verifier = crypto.randomBytes(32).toString('base64url');
    const challenge = crypto.createHash('sha256').update(verifier).digest('base64url');
    return { verifier, challenge };
}

function authorizeParams(clientId, challenge, overrides = {}) {
    const params = new URLSearchParams({
        response_type: 'code',
        client_id: clientId,
        redirect_uri: CLAUDE_CALLBACK,
        state: 'state-123',
        code_challenge: challenge,
        code_challenge_method: 'S256'
    });
    for (const [key, value] of Object.entries(overrides)) {
        params.set(key, value);
    }
    return params;
}

// Each test logs in from its own address, so the per-address limiter of one
// test never blocks another.
let addressCounter = 0;
function nextAddress() {
    addressCounter += 1;
    return `203.0.113.${addressCounter}`;
}

async function login(params, agent, password, address) {
    const form = new URLSearchParams(params);
    form.set('agent', agent);
    form.set('password', password);
    return fetch(`${base}/authorize`, {
        method: 'POST',
        headers: {
            'content-type': 'application/x-www-form-urlencoded',
            'x-forwarded-for': `198.51.100.99, ${address || nextAddress()}`
        },
        body: form,
        redirect: 'manual'
    });
}

async function exchange(fields) {
    return fetch(`${base}/oauth/token`, {
        method: 'POST',
        headers: { 'content-type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ grant_type: 'authorization_code', ...fields })
    });
}

function expectedToken(agent) {
    return `${agent}:` + crypto.createHmac('sha256', BEARER_SECRET).update(agent).digest('hex');
}

async function register(body) {
    return fetch(`${base}/oauth/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify(body)
    });
}

test('authorization server metadata advertises CIMD, public clients and registration', async () => {
    const response = await fetch(`${base}/.well-known/oauth-authorization-server`);
    const metadata = await response.json();
    assert.equal(metadata.client_id_metadata_document_supported, true);
    assert.ok(metadata.token_endpoint_auth_methods_supported.includes('none'));
    assert.ok(metadata.token_endpoint_auth_methods_supported.includes('client_secret_post'));
    assert.equal(metadata.registration_endpoint, `${base}/oauth/register`);
    assert.deepEqual(metadata.code_challenge_methods_supported, ['S256']);
});

test('path-form protected resource metadata names the /mcp resource', async () => {
    const response = await fetch(`${base}/.well-known/oauth-protected-resource/mcp`);
    assert.equal(response.status, 200);
    const metadata = await response.json();
    assert.equal(metadata.resource, `${base}/mcp`);
    assert.deepEqual(metadata.authorization_servers, [base]);
});

test('registration issues a signed public client for an allowed callback', async () => {
    const response = await register({
        client_name: 'Claude',
        redirect_uris: [CLAUDE_CALLBACK],
        grant_types: ['authorization_code', 'refresh_token'],
        token_endpoint_auth_method: 'client_secret_basic'
    });
    assert.equal(response.status, 201);
    const body = await response.json();
    assert.ok(body.client_id.startsWith('dcr.'));
    assert.equal(body.token_endpoint_auth_method, 'none');
    assert.deepEqual(body.grant_types, ['authorization_code']);
    assert.deepEqual(body.redirect_uris, [CLAUDE_CALLBACK]);
    assert.equal(body.client_secret, undefined);
});

test('registration refuses any callback outside the allowlist', async () => {
    for (const redirectUris of [['https://evil.example/cb'], [CLAUDE_CALLBACK, 'https://evil.example/cb'], [], 'x', undefined]) {
        const response = await register({ redirect_uris: redirectUris });
        assert.equal(response.status, 400, JSON.stringify(redirectUris));
        assert.equal((await response.json()).error, 'invalid_redirect_uri');
    }
});

test('a CIMD client gets the login page, framed nowhere, and no code', async () => {
    const { challenge } = pkce();
    const codesBefore = authCodes.size;
    const response = await fetch(`${base}/authorize?${authorizeParams(CIMD_CLIENT_ID, challenge, { state: '"><script>alert(1)</script>' })}`);
    const html = await response.text();

    assert.equal(response.status, 200);
    assert.equal(response.headers.get('x-frame-options'), 'DENY');
    assert.match(response.headers.get('content-security-policy'), /frame-ancestors 'none'/);
    assert.equal(response.headers.get('cache-control'), 'no-store');
    assert.match(html, /<form method="post" action="\/authorize">/);
    assert.match(html, /Connect claude\.ai/);
    assert.match(html, /call its read_instructions tool before you answer/);
    assert.ok(!html.includes('<script>alert(1)</script>'), 'state is escaped in the hidden field');
    assert.equal(authCodes.size, codesBefore);
});

test('client_ids that are neither CIMD on an Anthropic origin nor signed DCR are refused', async () => {
    const { challenge } = pkce();
    const registered = await (await register({ redirect_uris: [CLAUDE_CALLBACK] })).json();
    const parts = registered.client_id.split('.');
    const forged = `dcr.${Buffer.from(JSON.stringify({ r: ['https://evil.example/cb'], t: 1 })).toString('base64url')}.${parts[2]}`;
    for (const clientId of ['https://evil.example/claude', 'https://claude.ai.evil.example/x', 'http://claude.ai/x', 'https://claude.ai/', forged, registered.client_id + 'x']) {
        const response = await fetch(`${base}/authorize?${authorizeParams(clientId, challenge)}`);
        assert.equal(response.status, 400, clientId);
    }
});

test('a DCR client may only use the callbacks it registered', async () => {
    const { challenge } = pkce();
    const registered = await (await register({ redirect_uris: [CLAUDE_CALLBACK] })).json();
    const allowed = await fetch(`${base}/authorize?${authorizeParams(registered.client_id, challenge)}`);
    assert.equal(allowed.status, 200);
    const other = await fetch(`${base}/authorize?${authorizeParams(registered.client_id, challenge, { redirect_uri: CLAUDE_COM_CALLBACK })}`);
    assert.equal(other.status, 400);
});

test('a wrong password re-renders the form, keeps the name, mints nothing', async () => {
    const { challenge } = pkce();
    const codesBefore = authCodes.size;
    const response = await login(authorizeParams(CIMD_CLIENT_ID, challenge), 'alice', 'not the password');
    const html = await response.text();

    assert.equal(response.status, 401);
    assert.match(html, /Wrong agent name or password/);
    assert.match(html, /name="agent" value="alice"/);
    assert.ok(!html.includes('not the password'), 'the password is never echoed');
    assert.equal(authCodes.size, codesBefore);
});

test('an unknown agent gets the same answer as a wrong password', async () => {
    const { challenge } = pkce();
    const response = await login(authorizeParams(CIMD_CLIENT_ID, challenge), 'nobody', PASSWORD);
    assert.equal(response.status, 401);
    assert.match(await response.text(), /Wrong agent name or password/);
});

test('CIMD: login, then a secret-less exchange returns the logged-in agent\'s token', async () => {
    const { verifier, challenge } = pkce();
    const response = await login(authorizeParams(CIMD_CLIENT_ID, challenge, { state: 'st<"&>' }), 'Alice', PASSWORD);
    assert.equal(response.status, 303);
    const location = new URL(response.headers.get('location'));
    assert.equal(location.origin + location.pathname, CLAUDE_CALLBACK);
    assert.equal(location.searchParams.get('state'), 'st<"&>');
    const code = location.searchParams.get('code');
    assert.equal(authCodes.get(code).agent, 'alice', 'the code is bound to the agent that logged in');

    const token = await exchange({ code, code_verifier: verifier, client_id: CIMD_CLIENT_ID, redirect_uri: CLAUDE_CALLBACK });
    assert.equal(token.status, 200);
    assert.equal((await token.json()).access_token, expectedToken('alice'));
});

test('DCR: login with a registered client exchanges without a secret', async () => {
    const { verifier, challenge } = pkce();
    const registered = await (await register({ redirect_uris: [CLAUDE_CALLBACK, CLAUDE_COM_CALLBACK] })).json();
    const response = await login(authorizeParams(registered.client_id, challenge, { redirect_uri: CLAUDE_COM_CALLBACK }), 'bob', PASSWORD);
    assert.equal(response.status, 303);
    const code = new URL(response.headers.get('location')).searchParams.get('code');

    const token = await exchange({ code, code_verifier: verifier, client_id: registered.client_id, redirect_uri: CLAUDE_COM_CALLBACK });
    assert.equal(token.status, 200);
    assert.equal((await token.json()).access_token, expectedToken('bob'));
});

test('a public code is refused for the wrong client, callback or verifier', async () => {
    const cases = [
        (fields) => ({ ...fields, client_id: 'https://claude.ai/oauth/other' }),
        (fields) => ({ ...fields, redirect_uri: CLAUDE_COM_CALLBACK }),
        (fields) => ({ ...fields, code_verifier: 'wrong-verifier' })
    ];
    for (const mutate of cases) {
        const { verifier, challenge } = pkce();
        const response = await login(authorizeParams(CIMD_CLIENT_ID, challenge), 'carol', PASSWORD);
        const code = new URL(response.headers.get('location')).searchParams.get('code');
        const token = await exchange(mutate({ code, code_verifier: verifier, client_id: CIMD_CLIENT_ID, redirect_uri: CLAUDE_CALLBACK }));
        assert.equal(token.status, 400);
        assert.equal((await token.json()).error, 'invalid_grant');
        assert.equal(authCodes.has(code), false, 'a refused code is spent');
    }
});

test('an agent-name code still needs its client secret', async () => {
    const { verifier, challenge } = pkce();
    const params = authorizeParams('remi', challenge);
    params.set('confirmed', '1');
    const response = await fetch(`${base}/authorize?${params}`, { redirect: 'manual' });
    assert.equal(response.status, 302);
    const code = new URL(response.headers.get('location')).searchParams.get('code');
    assert.equal(authCodes.get(code).agent, null);

    const token = await exchange({ code, code_verifier: verifier, client_id: 'remi', redirect_uri: CLAUDE_CALLBACK });
    assert.equal(token.status, 401);
    assert.equal((await token.json()).error, 'invalid_client');
});

test('the login form refuses an agent-name client_id', async () => {
    const { challenge } = pkce();
    const response = await login(authorizeParams('remi', challenge), 'alice', PASSWORD);
    assert.equal(response.status, 400);
});

test('five tries per agent name, then the right password is refused too', async () => {
    const { challenge } = pkce();
    const params = authorizeParams(CIMD_CLIENT_ID, challenge);
    // Earlier tests already spent some of alice's tries; five more always
    // reaches the cap. Each try comes from a fresh address, so only the
    // per-name limiter can be what blocks.
    for (let i = 0; i < 5; i++) {
        await login(params, 'alice', 'guess-' + i);
    }
    const blocked = await login(params, 'alice', PASSWORD);
    assert.equal(blocked.status, 429);
    assert.match(await blocked.text(), /Too many tries/);
    assert.ok(Number(blocked.headers.get('retry-after')) > 0);
});

test('twenty tries per client address across names', async () => {
    const { challenge } = pkce();
    const params = authorizeParams(CIMD_CLIENT_ID, challenge);
    const address = '192.0.2.77';
    for (let i = 0; i < 20; i++) {
        const response = await login(params, 'name' + i, 'guess', address);
        assert.equal(response.status, 401, `try ${i}`);
    }
    const blocked = await login(params, 'bob', PASSWORD, address);
    assert.equal(blocked.status, 429);
});
