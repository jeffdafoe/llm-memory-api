// Tests for linkAccount (LLM-670) — the account-link decision logic, with
// every side effect faked. Run with: node --test (from node/api).

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { linkAccount } = require('./account-link');
const { createAttemptLimiter, acquireAttempt } = require('./attempt-limiter');

const CALLER = 10;
const TARGET = { id: 20, name: 'bob', password_hash: 'H', password_salt: 'S' };

// Fake deps. accounts: username -> row (only accounts WITH a dashboard
// password, as the real query filters). calls: every side effect, in order.
function fakeDeps(opts) {
    const o = Object.assign({
        accounts: { bob: TARGET },
        correctPassword: 'right',
        realm: true,
        existingRows: [],          // [[actorId, targetId], ...] already present
        allowAttempt: true,
        insertThrows: false,
    }, opts || {});
    const calls = [];
    const rows = o.existingRows.map(r => r.join('>'));
    const deps = {
        acquireAttempt: (callerId, username) => {
            calls.push(['acquire', callerId, username]);
            return o.allowAttempt ? { allowed: true } : { allowed: false, retryAfterSeconds: 600 };
        },
        findTarget: async (username) => {
            calls.push(['findTarget', username]);
            return o.accounts[username] || null;
        },
        verifyPassword: async (password, salt, hash) => {
            calls.push(['verify', salt, hash]);
            return password === o.correctPassword;
        },
        dummyHash: async () => {
            calls.push(['dummyHash']);
        },
        sharesRealm: async (a, b) => {
            calls.push(['sharesRealm', a, b]);
            return o.realm;
        },
        insertLinkRows: async (a, b) => {
            calls.push(['insert', a, b]);
            if (o.insertThrows) {
                throw new Error('db down');
            }
            let added = 0;
            for (const key of [a + '>' + b, b + '>' + a]) {
                if (!rows.includes(key)) {
                    rows.push(key);
                    added += 1;
                }
            }
            return added;
        },
        clearVisibilityCache: (id) => calls.push(['clearCache', id]),
        log: (event, details) => calls.push(['log', event, details]),
    };
    return { deps, calls, rows };
}

function names(calls) {
    return calls.map(c => c[0]);
}

function writes(calls) {
    return calls.filter(c => c[0] === 'insert' || c[0] === 'clearCache');
}

test('success inserts both directions, then clears both caches', async () => {
    const { deps, calls, rows } = fakeDeps();
    const result = await linkAccount({ callerId: CALLER, username: 'bob', password: 'right' }, deps);
    assert.equal(result.status, 200);
    assert.deepEqual(result.body, { linked: 'bob', already_linked: false });
    assert.deepEqual(rows.sort(), ['10>20', '20>10']);
    const order = names(calls);
    assert.ok(order.indexOf('insert') < order.indexOf('clearCache'), 'caches cleared only after the insert');
    assert.deepEqual(calls.filter(c => c[0] === 'clearCache').map(c => c[1]).sort(), [CALLER, TARGET.id]);
});

test('unknown account, account with no password, and wrong password give identical responses', async () => {
    const unknown = fakeDeps();
    const r1 = await linkAccount({ callerId: CALLER, username: 'nobody', password: 'x' }, unknown.deps);
    // An account with no dashboard password is filtered out by the real
    // query, so to the handler it is indistinguishable from "unknown".
    const noPassword = fakeDeps({ accounts: {} });
    const r2 = await linkAccount({ callerId: CALLER, username: 'bob', password: 'x' }, noPassword.deps);
    const wrong = fakeDeps();
    const r3 = await linkAccount({ callerId: CALLER, username: 'bob', password: 'wrong' }, wrong.deps);
    assert.deepEqual(r1, r2);
    assert.deepEqual(r1, r3);
    assert.equal(r1.status, 400, 'not 401 — the dashboard logs out on any 401');
    assert.equal(r1.body.error.code, 'INVALID_CREDENTIALS');
    // Equal hashing work: a dummy hash when there is no account, a real verify otherwise.
    assert.deepEqual(names(unknown.calls).filter(n => n === 'dummyHash' || n === 'verify'), ['dummyHash']);
    assert.deepEqual(names(wrong.calls).filter(n => n === 'dummyHash' || n === 'verify'), ['verify']);
    for (const f of [unknown, noPassword, wrong]) {
        assert.deepEqual(writes(f.calls), [], 'no insert, no cache clear');
        assert.ok(f.calls.some(c => c[0] === 'log' && c[1] === 'account_link_failed'));
    }
});

test('self-link writes nothing', async () => {
    const { deps, calls } = fakeDeps({ accounts: { me: { id: CALLER, name: 'me', password_hash: 'H', password_salt: 'S' } } });
    const result = await linkAccount({ callerId: CALLER, username: 'me', password: 'right' }, deps);
    assert.equal(result.status, 400);
    assert.deepEqual(writes(calls), []);
});

test('no shared realm returns 409 and writes nothing', async () => {
    const { deps, calls } = fakeDeps({ realm: false });
    const result = await linkAccount({ callerId: CALLER, username: 'bob', password: 'right' }, deps);
    assert.equal(result.status, 409);
    assert.equal(result.body.error.code, 'NO_SHARED_REALM');
    assert.deepEqual(writes(calls), []);
});

test('a half-existing link adds the missing direction and is not "already linked"', async () => {
    const { deps, rows } = fakeDeps({ existingRows: [[CALLER, TARGET.id]] });
    const result = await linkAccount({ callerId: CALLER, username: 'bob', password: 'right' }, deps);
    assert.equal(result.status, 200);
    assert.equal(result.body.already_linked, false);
    assert.deepEqual(rows.sort(), ['10>20', '20>10']);
});

test('a repeat link reports already_linked', async () => {
    const { deps } = fakeDeps({ existingRows: [[CALLER, TARGET.id], [TARGET.id, CALLER]] });
    const result = await linkAccount({ callerId: CALLER, username: 'bob', password: 'right' }, deps);
    assert.equal(result.status, 200);
    assert.equal(result.body.already_linked, true);
});

test('rate-limited: 429 with Retry-After, and no lookup or hashing at all', async () => {
    const { deps, calls } = fakeDeps({ allowAttempt: false });
    const result = await linkAccount({ callerId: CALLER, username: 'bob', password: 'right' }, deps);
    assert.equal(result.status, 429);
    assert.equal(result.headers['Retry-After'], '600');
    assert.deepEqual(names(calls), ['acquire']);
});

test('missing fields fail before an attempt is reserved', async () => {
    const { deps, calls } = fakeDeps();
    const result = await linkAccount({ callerId: CALLER, username: null, password: 'x' }, deps);
    assert.equal(result.status, 400);
    assert.deepEqual(calls, []);
});

test('a failed insert propagates and clears no cache', async () => {
    const { deps, calls } = fakeDeps({ insertThrows: true });
    await assert.rejects(linkAccount({ callerId: CALLER, username: 'bob', password: 'right' }, deps));
    assert.deepEqual(calls.filter(c => c[0] === 'clearCache'), []);
});

test('the attempt is reserved before the password check (parallel burst)', async () => {
    // Real limiter behind the handler: 20 requests launched together, all
    // with wrong passwords. Only the caller quota (5) may reach the password
    // check — the rest must be refused at acquire.
    const caller = createAttemptLimiter({ maxAttempts: 5, windowMs: 60000 });
    const target = createAttemptLimiter({ maxAttempts: 10, windowMs: 60000 });
    const { deps, calls } = fakeDeps();
    deps.acquireAttempt = (c, u) => acquireAttempt([[caller, c], [target, u]]);
    const results = await Promise.all(Array.from({ length: 20 }, () =>
        linkAccount({ callerId: CALLER, username: 'bob', password: 'wrong' }, deps)));
    assert.equal(results.filter(r => r.status === 429).length, 15);
    assert.equal(calls.filter(c => c[0] === 'verify').length, 5);
});

test('a correct link does not refund earlier guesses', async () => {
    const caller = createAttemptLimiter({ maxAttempts: 5, windowMs: 60000 });
    const target = createAttemptLimiter({ maxAttempts: 10, windowMs: 60000 });
    const { deps } = fakeDeps({ accounts: { bob: TARGET, v1: null, v2: null, v3: null, v4: null } });
    deps.acquireAttempt = (c, u) => acquireAttempt([[caller, c], [target, u]]);
    for (const victim of ['v1', 'v2', 'v3', 'v4']) {
        assert.equal((await linkAccount({ callerId: CALLER, username: victim, password: 'guess' }, deps)).status, 400);
    }
    assert.equal((await linkAccount({ callerId: CALLER, username: 'bob', password: 'right' }, deps)).status, 200);
    assert.equal((await linkAccount({ callerId: CALLER, username: 'v5', password: 'guess' }, deps)).status, 429);
});
