// Real-Postgres tests for the signup funnel (LLM-734). Opt-in: skipped unless
// TEST_DATABASE_URL names a SCRATCH database that has the production schema
// with migration MEM-150 applied. Never point it at a live database — the
// tests insert and delete accounts.
//
//   TEST_DATABASE_URL=postgres://user:pass@localhost:5432/scratch node --test src/services/signup-funnel.pg.test.js
//
// These cover what the stubbed tests cannot: the per-account lock holding the
// event cap under a parallel burst, and the tracking boundary at sub-second
// precision.

const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');

const TEST_DATABASE_URL = process.env.TEST_DATABASE_URL;
const skip = TEST_DATABASE_URL ? false : 'TEST_DATABASE_URL not set';

let pool;
let funnel;
const createdIds = [];

// Fresh, unique account names so a rerun never collides with leftovers.
const suffix = Date.now().toString(36);

// createdAt / offset are computed in SQL so microseconds survive (a JS Date
// keeps only milliseconds).
async function createAccount(name, createdAt, offset) {
    const result = await pool.query(
        `INSERT INTO actors (name, created_at)
         VALUES ($1, COALESCE($2::timestamptz, NOW()) - COALESCE($3::interval, INTERVAL '0'))
         RETURNING id`,
        [name + '-' + suffix, createdAt || null, offset || null]
    );
    const id = result.rows[0].id;
    createdIds.push(id);
    await pool.query('UPDATE actors SET created_by = id WHERE id = $1', [id]);
    await pool.query('INSERT INTO agent_configuration (actor_id, virtual) VALUES ($1, false)', [id]);
    return id;
}

// stampToolCall does not wait for its UPDATE; poll until it lands.
async function readStamps(id) {
    for (let i = 0; i < 50; i++) {
        const result = await pool.query(
            'SELECT first_tool_call_at, tool_call_count FROM actors WHERE id = $1',
            [id]
        );
        if (result.rows[0].tool_call_count > 0) {
            return result.rows[0];
        }
        await new Promise((resolve) => setTimeout(resolve, 20));
    }
    throw new Error('tool-call stamp never landed');
}

before(async () => {
    if (skip) return;
    // db.js reads DATABASE_URL when it is first required.
    process.env.DATABASE_URL = TEST_DATABASE_URL;
    pool = require('../db');
    const config = require('./config');
    await config.init();
    funnel = require('./signup-funnel');
    assert.ok(funnel.trackingSince(), 'MEM-150 applied: config.signup_funnel_since is set');
});

after(async () => {
    if (skip) return;
    if (createdIds.length) {
        await pool.query('DELETE FROM agent_configuration WHERE actor_id = ANY($1)', [createdIds]);
        await pool.query('DELETE FROM actors WHERE id = ANY($1)', [createdIds]);
    }
    await pool.end();
});

test('the event cap holds under a parallel burst', { skip }, async () => {
    const id = await createAccount('burst');
    for (let i = 0; i < 5; i++) {
        assert.equal((await funnel.recordSignupEvent(id, 'copy', 'mcp_url')).recorded, true);
    }
    const burst = await Promise.all(
        Array.from({ length: 300 }, () => funnel.recordSignupEvent(id, 'copy', 'api_key'))
    );
    const count = await pool.query('SELECT COUNT(*)::int AS n FROM signup_events WHERE actor_id = $1', [id]);
    assert.equal(count.rows[0].n, funnel.MAX_EVENTS_PER_ACCOUNT);
    assert.equal(burst.filter((r) => r.recorded).length, funnel.MAX_EVENTS_PER_ACCOUNT - 5);
});

test('an account created just before the tracking start never gets a first stamp', { skip }, async () => {
    const since = funnel.trackingSince();
    assert.match(since, /\.\d{6}Z$/, 'MEM-150 stores the start with microseconds');
    // Same second, one microsecond earlier: a start truncated to the second
    // would wrongly count this account as tracked.
    const before = await createAccount('edgebefore', since, '1 microsecond');
    const at = await createAccount('edgeat', since);
    funnel.stampToolCall(before);
    funnel.stampToolCall(at);
    assert.equal((await readStamps(before)).first_tool_call_at, null);
    assert.ok((await readStamps(at)).first_tool_call_at);
});
