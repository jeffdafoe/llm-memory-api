// Tests for POST /api/register invite-code handling (LLM-735). Run with:
// node --test (from node/api). Uses node:test + node:assert, matching
// oauth.test.js.
//
// The router is mounted on a throwaway Express app on a random port. The db
// pool, config, name checks, mail and note saving are stubbed before
// registration.js is required — it destructures those helpers at require
// time. Every SQL statement is recorded so a test can see whether the invite
// was marked used and which email reached the new actor row.

const { test, before, after, beforeEach } = require('node:test');
const assert = require('node:assert/strict');
const express = require('express');

const pool = require('../db');
const config = require('../services/config');
const actors = require('../services/actors');
const mail = require('../services/mail');
const documents = require('../services/documents');

const FUTURE = new Date(Date.now() + 86400000).toISOString();
const PAST = new Date(Date.now() - 86400000).toISOString();

// Invite rows by code. 'raced' is unused at the Phase 1 read and used by the
// time the Phase 3 FOR UPDATE read runs, as if a concurrent signup took it.
const INVITES = {
    good: { id: 1, used_by: null, expires_at: FUTURE, realm: 'llm-memory', access_request_id: 7 },
    used: { id: 2, used_by: 'someone', expires_at: FUTURE, realm: 'llm-memory', access_request_id: 8 },
    stale: { id: 3, used_by: null, expires_at: PAST, realm: 'llm-memory', access_request_id: 9 },
    raced: { id: 4, used_by: null, expires_at: FUTURE, realm: 'llm-memory', access_request_id: 10 }
};
const RACED_AT_LOCK = { ...INVITES.raced, used_by: 'someone-else' };

let openRegistration = true;
let statements = [];

config.get = (key) => {
    if (key === 'open_registration') return openRegistration ? 'true' : 'false';
    if (key === 'minimum_password_length') return '10';
    return undefined;
};
actors.checkNameAvailability = async () => ({ available: true });
actors.moderateActorName = async () => ({ approved: true });
mail.mailSend = async () => {};
documents.saveNote = async () => {};

function answer(sql, params) {
    statements.push({ sql, params });
    if (/FROM invite_codes WHERE code = \$1/.test(sql)) {
        const invite = INVITES[params[0]];
        return { rows: invite ? [invite] : [] };
    }
    if (/FROM invite_codes WHERE id = \$1 FOR UPDATE/.test(sql)) {
        const invite = Object.values(INVITES).find((row) => row.id === params[0]);
        return { rows: [invite === INVITES.raced ? RACED_AT_LOCK : invite] };
    }
    if (/SELECT email FROM access_requests/.test(sql)) {
        return { rows: [{ email: `request-${params[0]}@example.com` }] };
    }
    if (/SELECT id FROM actors WHERE name/.test(sql)) {
        return { rows: [{ id: 99 }] };
    }
    return { rows: [] };
}

pool.query = async (sql, params) => answer(sql, params);
pool.connect = async () => ({
    query: async (sql, params) => answer(sql, params),
    release: () => {}
});

const registrationRouter = require('./registration');

let server;
let base;

before(async () => {
    const app = express();
    app.use(express.json());
    app.use(registrationRouter);
    await new Promise((resolve) => {
        server = app.listen(0, '127.0.0.1', resolve);
    });
    base = `http://127.0.0.1:${server.address().port}`;
});

after(() => {
    server.close();
});

beforeEach(() => {
    openRegistration = true;
    statements = [];
});

async function register(code) {
    const body = { name: 'newcomer', password: 'long-enough-password', dream_mode: 'none' };
    if (code !== undefined) body.code = code;
    const res = await fetch(`${base}/api/register`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
    });
    return { status: res.status, body: await res.json() };
}

// The email the INSERT INTO actors carried (its 7th parameter).
function actorEmail() {
    const insert = statements.find((s) => /INSERT INTO actors/.test(s.sql));
    assert.ok(insert, 'an actor row was inserted');
    return insert.params[6];
}

function inviteMarkedUsed() {
    const update = statements.find((s) => /UPDATE invite_codes SET used_by/.test(s.sql));
    return update ? update.params[1] : null;
}

test('open registration with a good code links the account to its request', async () => {
    const res = await register('good');
    assert.equal(res.status, 200);
    assert.equal(inviteMarkedUsed(), 1);
    assert.equal(actorEmail(), 'request-7@example.com');
});

test('open registration drops a used, expired or unknown code instead of refusing', async () => {
    for (const code of ['used', 'stale', 'no-such-code']) {
        statements = [];
        const res = await register(code);
        assert.equal(res.status, 200, `code ${code}`);
        assert.equal(inviteMarkedUsed(), null, `code ${code}`);
        assert.equal(actorEmail(), null, `code ${code}`);
    }
});

test('open registration that loses the code under the row lock registers code-less', async () => {
    const res = await register('raced');
    assert.equal(res.status, 200);
    assert.equal(inviteMarkedUsed(), null);
    assert.equal(actorEmail(), null);
    assert.ok(!statements.some((s) => /access_requests/.test(s.sql)), 'no email read for a lost code');
});

test('open registration without a code still works', async () => {
    const res = await register();
    assert.equal(res.status, 200);
    assert.equal(inviteMarkedUsed(), null);
    assert.equal(actorEmail(), null);
});

test('invite-only registration still refuses a bad code', async () => {
    openRegistration = false;
    const cases = {
        used: 'This invite code has already been used',
        stale: 'This invite code has expired',
        'no-such-code': 'Invalid invite code'
    };
    for (const [code, error] of Object.entries(cases)) {
        statements = [];
        const res = await register(code);
        assert.equal(res.status, 400, `code ${code}`);
        assert.equal(res.body.error, error);
        assert.ok(!statements.some((s) => /INSERT INTO actors/.test(s.sql)), `code ${code}`);
    }
});

test('invite-only registration refuses a code lost under the row lock', async () => {
    openRegistration = false;
    const res = await register('raced');
    assert.equal(res.status, 400);
    assert.equal(res.body.error, 'This invite code has already been used');
    assert.ok(statements.some((s) => s.sql === 'ROLLBACK'));
    assert.ok(!statements.some((s) => /INSERT INTO actors/.test(s.sql)));
});
