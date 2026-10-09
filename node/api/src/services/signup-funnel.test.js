// Tests for the signup funnel service (LLM-734). Run with: node --test (from
// node/api). Uses node:test + node:assert, matching the other service tests.
//
// The db pool and config are stubbed before signup-funnel.js is required, so
// nothing here touches Postgres. The SQL itself was checked against a scratch
// copy of the schema (see the PR); these tests cover the JS around it: input
// validation, the tracking start, row mapping and the stage counts.

const { test, beforeEach } = require('node:test');
const assert = require('node:assert/strict');

const pool = require('../db');
const config = require('./config');

let configValues = {};
let queries = [];
let nextRows = [];

config.get = (key) => configValues[key];
pool.query = async (sql, params) => {
    queries.push({ sql, params });
    return { rows: nextRows };
};

const funnel = require('./signup-funnel');

const DAY = 86400000;
const SINCE = '2026-10-09T21:00:00Z';

beforeEach(() => {
    configValues = { signup_funnel_since: SINCE };
    queries = [];
    nextRows = [];
});

test('trackingSince reads the config row, and is null when missing or not a date', () => {
    assert.equal(funnel.trackingSince(), SINCE);
    configValues = {};
    assert.equal(funnel.trackingSince(), null);
    configValues = { signup_funnel_since: 'soon' };
    assert.equal(funnel.trackingSince(), null);
});

test('recordSignupEvent refuses an unknown event or a malformed value', async () => {
    await assert.rejects(funnel.recordSignupEvent(1, 'clicked', 'claude-ai'), { statusCode: 400 });
    for (const value of [undefined, '', 'Claude-AI', '1tab', 'a'.repeat(33), 'claude ai', 42]) {
        await assert.rejects(funnel.recordSignupEvent(1, 'client', value), { statusCode: 400 }, String(value));
    }
    assert.equal(queries.length, 0, 'nothing reaches the database');
});

test('recordSignupEvent stores a valid event, and reports a dropped one without failing', async () => {
    nextRows = [{ id: 9 }];
    assert.deepEqual(await funnel.recordSignupEvent(5, 'copy', 'mcp_url'), { recorded: true });
    assert.deepEqual(queries[0].params, [5, 'copy', 'mcp_url', funnel.SIGNUP_WINDOW_HOURS, funnel.MAX_EVENTS_PER_ACCOUNT]);

    nextRows = [];
    const dropped = await funnel.recordSignupEvent(5, 'copy', 'mcp_url');
    assert.equal(dropped.recorded, false);
});

test('stampToolCall passes the tracking start, and skips an unknown actor', async () => {
    funnel.stampToolCall(7);
    assert.deepEqual(queries[0].params, [7, SINCE]);
    funnel.stampToolCall(undefined);
    assert.equal(queries.length, 1);
});

test('activeAfter is unknown until the account is old enough', () => {
    const now = Date.now();
    const young = { created_at: new Date(now - 3 * DAY), last_tool_call_at: new Date(now) };
    assert.equal(funnel.activeAfter(young, 7), null);

    const lapsed = { created_at: new Date(now - 40 * DAY), last_tool_call_at: new Date(now - 39 * DAY) };
    assert.equal(funnel.activeAfter(lapsed, 7), false);
    assert.equal(funnel.activeAfter(lapsed, 30), false);

    const kept = { created_at: new Date(now - 40 * DAY), last_tool_call_at: new Date(now - DAY) };
    assert.equal(funnel.activeAfter(kept, 30), true);

    const never = { created_at: new Date(now - 40 * DAY), last_tool_call_at: null };
    assert.equal(funnel.activeAfter(never, 7), false);
});

function dbRow(overrides) {
    return {
        id: 1, name: 'someone', email: null, created_at: new Date(Date.now() - DAY),
        first_connected_at: null, first_tool_call_at: null, last_tool_call_at: null, tool_call_count: 0,
        tracked: true, picked_client: null, shown_client: null, copies: [], guide_shown_at: null,
        key_last_used_at: null,
        ...overrides
    };
}

test('funnelRows: a tracked account is connected only by an MCP connection, not by key use', async () => {
    // The guide's own events authenticate with the new key, so a tracked
    // account always has a key use before it connects anything.
    nextRows = [dbRow({ key_last_used_at: new Date() })];
    let [row] = await funnel.funnelRows();
    assert.equal(row.connected, false);

    nextRows = [dbRow({ first_connected_at: new Date() })];
    [row] = await funnel.funnelRows();
    assert.equal(row.connected, true);
    assert.deepEqual(queries[0].params, [SINCE]);
});

test('funnelRows: an account from before tracking falls back to key use', async () => {
    nextRows = [dbRow({ tracked: false, key_last_used_at: new Date() })];
    const [row] = await funnel.funnelRows();
    assert.equal(row.tracked, false);
    assert.equal(row.connected, true);
});

test('funnelRows: client is the last tab picked, else the tab the guide opened on', async () => {
    nextRows = [
        dbRow({ name: 'picked', picked_client: 'claude-ai', shown_client: 'claude-code' }),
        dbRow({ name: 'default', shown_client: 'claude-code' }),
        dbRow({ name: 'noguide' })
    ];
    const rows = await funnel.funnelRows();
    assert.deepEqual(rows.map((r) => [r.name, r.client, r.client_picked]), [
        ['picked', 'claude-ai', true],
        ['default', 'claude-code', false],
        ['noguide', null, false]
    ]);
});

test('funnelSummary counts each stage and only accounts old enough for 7 / 30 days', () => {
    const rows = [
        { guide_shown_at: new Date(), client_picked: true, connected: true, first_tool_call_at: new Date(), tool_call_count: 3, active_after_7_days: true, active_after_30_days: null },
        { guide_shown_at: new Date(), client_picked: false, connected: true, first_tool_call_at: null, tool_call_count: 0, active_after_7_days: false, active_after_30_days: false },
        { guide_shown_at: null, client_picked: false, connected: false, first_tool_call_at: null, tool_call_count: 0, active_after_7_days: null, active_after_30_days: null }
    ];
    assert.deepEqual(funnel.funnelSummary(rows), {
        signed_up: 3,
        saw_guide: 2,
        picked_client: 1,
        connected: 2,
        first_tool_call: 1,
        active_after_7_days: 1,
        old_enough_for_7_days: 2,
        active_after_30_days: 0,
        old_enough_for_30_days: 1
    });
});
