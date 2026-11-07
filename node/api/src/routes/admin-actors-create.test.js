// Route tests for POST /admin/actors/create claiming the new actor's
// namespace (LLM-931). Run with: node --test (from node/api).
//
// The admin router is mounted on a throwaway Express app; a stand-in for the
// auth middleware sets the caller. The db pool is stubbed and records every
// statement. The availability check and moderation are stubbed before
// admin.js is required (it destructures them), so the check passes and only
// the claim inside the transaction can see a note in the namespace.

const { test, before, after, beforeEach } = require('node:test');
const assert = require('node:assert/strict');
const express = require('express');

const pool = require('../db');
const actors = require('../services/actors');

let statements = [];
// True when a note lands in the new name's namespace after the availability
// check, so only the re-check under the documents lock can see it.
let namespaceTakenAtLock = false;

actors.checkNameAvailability = async () => ({ available: true });
actors.moderateActorName = async () => ({ approved: true });

function answer(sql, params) {
    statements.push({ sql, params });
    if (/FROM admin_permissions/.test(sql)) {
        return { rows: [{ resource: 'agents', action: 'write' }] };
    }
    if (/SELECT realms, email FROM actors WHERE id/.test(sql)) {
        return { rows: [{ realms: ['contract'], email: null }] };
    }
    if (/SELECT 1 FROM documents WHERE namespace = \$1/.test(sql)) {
        return { rows: namespaceTakenAtLock ? [{ '?column?': 1 }] : [] };
    }
    if (/INSERT INTO actors/.test(sql)) {
        return { rows: [{ id: 99 }] };
    }
    return { rows: [] };
}

pool.query = async (sql, params) => answer(sql, params);
pool.connect = async () => ({
    query: async (sql, params) => answer(sql, params),
    release: () => {}
});

const adminRouter = require('./admin');

let server;
let base;

before(async () => {
    const app = express();
    app.use(express.json());
    app.use((req, res, next) => {
        req.actorId = 2;
        req.authenticatedUser = { id: 2, username: 'admin' };
        next();
    });
    app.use(adminRouter);
    await new Promise((resolve) => {
        server = app.listen(0, '127.0.0.1', resolve);
    });
    base = `http://127.0.0.1:${server.address().port}`;
});

after(() => {
    server.close();
});

beforeEach(() => {
    statements = [];
    namespaceTakenAtLock = false;
});

async function create() {
    const res = await fetch(`${base}/admin/actors/create`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: 'newcomer' })
    });
    return { status: res.status, body: await res.json() };
}

// Where each step of the claim sits in the recorded statements. Every step
// must be there: a missing one fails here, before any order is compared.
function claimOrder() {
    const sqls = statements.map((s) => s.sql);
    const find = (label, match) => {
        const at = sqls.findIndex(match);
        assert.ok(at >= 0, `${label} was run`);
        return at;
    };
    return {
        begin: find('BEGIN', (sql) => sql === 'BEGIN'),
        lock: find('the documents lock', (sql) => sql === 'LOCK TABLE documents IN SHARE MODE'),
        recheck: find('the namespace re-check', (sql) => /SELECT 1 FROM documents WHERE namespace/.test(sql)),
        insert: sqls.findIndex((sql) => /INSERT INTO actors/.test(sql)),
        commit: sqls.indexOf('COMMIT')
    };
}

test('a namespace taken after the availability check is refused under the documents lock', async () => {
    namespaceTakenAtLock = true;
    const res = await create();
    assert.equal(res.status, 409);
    assert.deepEqual(res.body, { error: { code: 'ALREADY_EXISTS', message: 'This name conflicts with an existing namespace.' } });
    const { begin, lock, recheck, insert } = claimOrder();
    assert.ok(begin < lock && lock < recheck, 'the re-check runs inside the transaction, after the lock');
    assert.ok(statements.some((s) => s.sql === 'ROLLBACK'));
    assert.equal(insert, -1, 'no actor row');
});

test('actor create takes the documents lock before inserting the actor', async () => {
    const res = await create();
    assert.equal(res.status, 200);
    const { begin, lock, recheck, insert, commit } = claimOrder();
    assert.ok(begin < lock && lock < recheck && recheck < insert && insert < commit,
        'BEGIN, lock, re-check, actor insert, COMMIT in that order');
});
