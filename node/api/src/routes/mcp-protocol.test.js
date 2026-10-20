// Protocol-level tests for /mcp on the v2 MCP SDK (LLM-731). Run with:
// node --test (from node/api). Uses node:test + node:assert, matching the
// other route tests.
//
// The router is mounted on a throwaway Express app on a random port, with
// express.json app-wide as server.js has it. Requests carry a real HMAC
// bearer, so the real mcpAuth middleware runs; the db pool, actor lookup and
// config are stubbed before mcp.js is required. Two clients are exercised:
// the v2 SDK client negotiating the 2026-07-28 era (server/discover), and
// raw 2025-era JSON-RPC posts (the initialize handshake Claude Code and
// claude.ai fall back to).

const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const crypto = require('crypto');
const express = require('express');
const { Client, StreamableHTTPClientTransport } = require('@modelcontextprotocol/client');

const pool = require('../db');
const actors = require('../services/actors');
const apiKeys = require('../services/api-keys');
const config = require('../services/config');

const BEARER_SECRET = 'test-secret';
const REMI_API_KEY = 'a'.repeat(64);
const ACTORS = {
    remi: { id: 1, permissions: ['mcp_read_note', 'mcp_agent_status'] },
    limited: { id: 2, permissions: ['mcp_read_note'] },
    quiet: { id: 3, permissions: ['mcp_read_note'] }
};

// Every UPDATE the route and handlers run, so the spinner write can be checked.
const updates = [];

config.get = (key) => (key === 'mcp_oauth_bearer_secret' ? BEARER_SECRET : undefined);
apiKeys.findApiKeyByToken = async (token) => {
    if (token === REMI_API_KEY) {
        return { agent: 'remi', actorId: ACTORS.remi.id };
    }
    return null;
};
actors.resolveByName = async (name) => {
    if (!ACTORS[name]) {
        return null;
    }
    return { id: ACTORS[name].id, name };
};
pool.query = async (sql, params) => {
    if (/FROM agent_permissions/.test(sql)) {
        const actor = Object.values(ACTORS).find((a) => a.id === params[0]);
        return { rows: actor.permissions.map((name) => ({ name })) };
    }
    if (/^\s*UPDATE actors/.test(sql)) {
        updates.push({ sql, params });
    }
    return { rows: [] };
};

const mcpRouter = require('./mcp');

let server;
let url;

before(async () => {
    const app = express();
    app.use(express.json({ limit: '5mb' }));
    app.use(mcpRouter);
    await new Promise((resolve) => {
        server = app.listen(0, '127.0.0.1', resolve);
    });
    url = `http://127.0.0.1:${server.address().port}/mcp`;
});

after(() => {
    server.close();
});

function bearer(agent) {
    return agent + ':' + crypto.createHmac('sha256', BEARER_SECRET).update(agent).digest('hex');
}

// A 2025-era JSON-RPC post. The SDK answers legacy POSTs as an SSE stream
// carrying one `data:` frame; the frame is parsed out here.
async function legacyPost(agent, body, extraHeaders = {}) {
    const headers = Object.assign({
        'content-type': 'application/json',
        accept: 'application/json, text/event-stream',
        authorization: 'Bearer ' + bearer(agent)
    }, extraHeaders);
    const response = await fetch(url, { method: 'POST', headers, body: JSON.stringify(body) });
    const text = await response.text();
    const dataLine = text.split('\n').find((line) => line.startsWith('data: '));
    let message = null;
    if (dataLine) {
        message = JSON.parse(dataLine.slice('data: '.length));
    }
    return { status: response.status, sessionId: response.headers.get('mcp-session-id'), message };
}

function initializeBody() {
    return {
        jsonrpc: '2.0',
        id: 1,
        method: 'initialize',
        params: { protocolVersion: '2025-11-25', capabilities: {}, clientInfo: { name: 'legacy-test', version: '1.0' } }
    };
}

// Collects console.log lines while fn runs.
async function captureLogs(fn) {
    const lines = [];
    const original = console.log;
    console.log = (...args) => {
        lines.push(args.join(' '));
    };
    try {
        await fn();
    } finally {
        console.log = original;
    }
    return lines;
}

test('a 2026-07-28 client negotiates the modern era and calls tools', async () => {
    const client = new Client({ name: 'modern-test', version: '2.0' }, { versionNegotiation: { mode: 'auto' } });
    const transport = new StreamableHTTPClientTransport(new URL(url), {
        requestInit: { headers: { authorization: 'Bearer ' + bearer('remi') } }
    });

    const logs = await captureLogs(() => client.connect(transport));

    assert.equal(client.getNegotiatedProtocolVersion(), '2026-07-28');
    assert.match(client.getInstructions(), /call the read_instructions tool/);

    const connectLine = logs.find((line) => line.startsWith('[mcp] connect'));
    const connect = JSON.parse(connectLine.slice('[mcp] connect '.length));
    assert.equal(connect.agent, 'remi');
    assert.equal(connect.era, 'modern');
    assert.equal(connect.method, 'server/discover');
    assert.equal(connect.protocolVersion, '2026-07-28');
    assert.equal(connect.clientName, 'modern-test');
    assert.ok(updates.some((u) => /active_since = NOW\(\)/.test(u.sql) && u.params[0] === 1));

    const { tools } = await client.listTools();
    const names = tools.map((t) => t.name);
    assert.ok(names.includes('read_note'));
    assert.ok(names.includes('activity_start'));
    assert.ok(!names.includes('save_note'));

    const result = await client.callTool({ name: 'activity_start', arguments: {} });
    assert.deepEqual(result.content, [{ type: 'text', text: 'Activity started for remi.' }]);

    await client.close();
});

test('a 2025-era client initializes statelessly and gets the instructions pointer', async () => {
    const logs = await captureLogs(async () => {
        // A client-supplied first hop must not reach the log; nginx appends the real peer last.
        const { status, sessionId, message } = await legacyPost('remi', initializeBody(),
            { 'x-forwarded-for': '127.0.0.1, 203.0.113.9' });
        assert.equal(status, 200);
        assert.equal(sessionId, null);
        assert.equal(message.result.protocolVersion, '2025-11-25');
        assert.match(message.result.instructions, /call the read_instructions tool/);
    });

    const connectLine = logs.find((line) => line.startsWith('[mcp] connect'));
    const connect = JSON.parse(connectLine.slice('[mcp] connect '.length));
    assert.equal(connect.era, 'legacy');
    assert.equal(connect.ip, '203.0.113.9');
    assert.equal(connect.method, 'initialize');
    assert.equal(connect.protocolVersion, '2025-11-25');
    assert.equal(connect.clientName, 'legacy-test');
});

test('a 2025-era call still holding a session ID from before a restart is served', async () => {
    const { status, message } = await legacyPost('remi', {
        jsonrpc: '2.0',
        id: 2,
        method: 'tools/call',
        params: { name: 'activity_start', arguments: {} }
    }, { 'mcp-protocol-version': '2025-11-25', 'mcp-session-id': 'session-from-before-the-deploy' });

    assert.equal(status, 200);
    assert.deepEqual(message.result.content, [{ type: 'text', text: 'Activity started for remi.' }]);
});

test('the same stale session ID carries no principal between agents', async () => {
    const staleSession = { 'mcp-protocol-version': '2025-11-25', 'mcp-session-id': 'shared-stale-session' };
    const callBody = { jsonrpc: '2.0', id: 5, method: 'tools/call', params: { name: 'activity_start', arguments: {} } };

    const first = await legacyPost('remi', callBody, staleSession);
    assert.deepEqual(first.message.result.content, [{ type: 'text', text: 'Activity started for remi.' }]);

    const second = await legacyPost('limited', callBody, staleSession);
    assert.equal(second.message.result.isError, true);
    assert.deepEqual(second.message.result.content, [{ type: 'text', text: 'Permission denied: mcp_agent_status' }]);

    const list = await legacyPost('limited', { jsonrpc: '2.0', id: 6, method: 'tools/list' }, staleSession);
    assert.ok(!list.message.result.tools.some((t) => t.name === 'activity_start'));
});

test('an API key authenticates the same principal as its agent', async () => {
    const { status, message } = await legacyPost('remi', {
        jsonrpc: '2.0',
        id: 7,
        method: 'tools/call',
        params: { name: 'activity_start', arguments: {} }
    }, { 'mcp-protocol-version': '2025-11-25', authorization: 'Bearer ' + REMI_API_KEY });

    assert.equal(status, 200);
    assert.deepEqual(message.result.content, [{ type: 'text', text: 'Activity started for remi.' }]);
});

test('a claim-less server/discover is refused and is not a connect', async () => {
    const before = updates.length;
    const logs = await captureLogs(async () => {
        const { status, message } = await legacyPost('quiet', { jsonrpc: '2.0', id: 8, method: 'server/discover' });
        assert.equal(status, 200);
        assert.equal(message.error.code, -32601);
    });

    assert.ok(!logs.some((line) => line.startsWith('[mcp] connect')));
    assert.ok(!updates.slice(before).some((u) => /active_since = NOW\(\)/.test(u.sql) && u.params[0] === ACTORS.quiet.id));
});

test('tool permissions filter tools/list and refuse tools/call', async () => {
    const list = await legacyPost('limited', { jsonrpc: '2.0', id: 3, method: 'tools/list' },
        { 'mcp-protocol-version': '2025-11-25' });
    const names = list.message.result.tools.map((t) => t.name);
    assert.ok(names.includes('read_note'));
    assert.ok(!names.includes('activity_start'));

    const call = await legacyPost('limited', {
        jsonrpc: '2.0',
        id: 4,
        method: 'tools/call',
        params: { name: 'activity_start', arguments: {} }
    }, { 'mcp-protocol-version': '2025-11-25' });
    assert.equal(call.message.result.isError, true);
    assert.deepEqual(call.message.result.content, [{ type: 'text', text: 'Permission denied: mcp_agent_status' }]);
});

test('GET and DELETE, the 2025 session operations, answer 405', async () => {
    const headers = { authorization: 'Bearer ' + bearer('remi'), 'mcp-session-id': 'any' };
    const get = await fetch(url, { method: 'GET', headers: Object.assign({ accept: 'text/event-stream' }, headers) });
    assert.equal(get.status, 405);
    await get.text();
    const del = await fetch(url, { method: 'DELETE', headers });
    assert.equal(del.status, 405);
    await del.text();
});

test('requests without a valid bearer are refused before the SDK', async () => {
    const missing = await fetch(url, {
        method: 'POST',
        headers: { 'content-type': 'application/json', accept: 'application/json, text/event-stream' },
        body: JSON.stringify(initializeBody())
    });
    assert.equal(missing.status, 401);
    assert.match(missing.headers.get('www-authenticate'), /resource_metadata=/);
    await missing.text();

    const forged = await fetch(url, {
        method: 'POST',
        headers: {
            'content-type': 'application/json',
            accept: 'application/json, text/event-stream',
            authorization: 'Bearer remi:' + '0'.repeat(64)
        },
        body: JSON.stringify(initializeBody())
    });
    assert.equal(forged.status, 401);
    await forged.text();
});

test('HEAD /mcp advertises a protocol version without auth', async () => {
    const response = await fetch(url, { method: 'HEAD' });
    assert.equal(response.status, 200);
    assert.equal(response.headers.get('mcp-protocol-version'), '2025-11-25');
});
