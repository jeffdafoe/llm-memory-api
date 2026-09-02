// Tests for the /agent/memory/sync prune decision (LLM-565). Run with:
// node --test (from node/api). Uses node:test + node:assert, matching
// sim.test.js — no test-framework dep.
//
// Prune mode makes the local memory directory authoritative for existence, so
// the branch that decides prune-vs-pull is the one place a bug destroys notes.
// There's no route/supertest harness in this repo, so the decision is extracted
// into two pure functions on the router and exercised directly here.

const { test } = require('node:test');
const assert = require('node:assert/strict');

// agent.js pulls in the db pool and several services at require time; none of
// them connect until a query runs, and these helpers touch neither.
const agentRouter = require('./agent');
const { resolvePruneCutoff, remoteOnlyAction } = agentRouter;

const NOW = Date.parse('2026-07-30T12:00:00.000Z');

test('resolvePruneCutoff places the cutoff on the server clock', () => {
    // 60s ago per the client's own elapsed measurement, subtracted from OUR now
    // — the client's absolute clock never enters the comparison, so an offset
    // in either direction cannot move the cutoff.
    const cutoff = resolvePruneCutoff(60000, NOW);
    assert.equal(cutoff, NOW - 60000);
});

test('resolvePruneCutoff accepts a scan age of zero', () => {
    // A scan fast enough to round to 0 ms is the ordinary case, not a missing
    // value — it must not be confused with one.
    assert.equal(resolvePruneCutoff(0, NOW), NOW);
});

test('resolvePruneCutoff rejects a missing or non-numeric scan age', () => {
    assert.equal(resolvePruneCutoff(undefined, NOW), null);
    assert.equal(resolvePruneCutoff(null, NOW), null);
    assert.equal(resolvePruneCutoff('60000', NOW), null);
    assert.equal(resolvePruneCutoff(NaN, NOW), null);
    assert.equal(resolvePruneCutoff(Infinity, NOW), null);
});

test('resolvePruneCutoff rejects a negative or absurdly old scan age', () => {
    // Negative means the client scanned in its own future. Past the ceiling the
    // cutoff would sit far enough back to spare notes that really were retired,
    // so we refuse rather than prune against a stale premise.
    assert.equal(resolvePruneCutoff(-1, NOW), null);
    assert.equal(resolvePruneCutoff(60 * 60 * 1000 + 1, NOW), null);
    assert.equal(resolvePruneCutoff(60 * 60 * 1000, NOW), NOW - 60 * 60 * 1000);
});

test('remote-only note pulls when prune is off', () => {
    // The historical behavior, and what every sync without the flag must keep
    // doing — an older note is still pulled, not deleted.
    const action = remoteOnlyAction(false, '2026-07-30T09:00:00.000Z', null);
    assert.equal(action, 'pull');
});

test('remote-only note prunes when it predates the local scan', () => {
    // The consolidation case: the file was retired locally, so its remote copy
    // is the stale side.
    const action = remoteOnlyAction(true, '2026-07-30T09:00:00.000Z', NOW);
    assert.equal(action, 'prune');
});

test('remote-only note prunes when it is exactly as old as the scan', () => {
    // Boundary: equal timestamps mean the note existed at scan time, so its
    // absence locally is a deletion, not a race.
    const action = remoteOnlyAction(true, '2026-07-30T12:00:00.000Z', NOW);
    assert.equal(action, 'prune');
});

test('remote-only note pulls when it was created after the local scan', () => {
    // A concurrent session wrote it in the window between our scan and this
    // request. Deleting it would destroy work we never saw.
    const action = remoteOnlyAction(true, '2026-07-30T12:00:01.000Z', NOW);
    assert.equal(action, 'pull');
});

test('remote-only note pulls when its remote timestamp is unusable', () => {
    // Fail closed: without a readable timestamp we cannot prove the note
    // predates the scan, so we keep it.
    assert.equal(remoteOnlyAction(true, null, NOW), 'pull');
    assert.equal(remoteOnlyAction(true, 'garbage', NOW), 'pull');
});

// ── classifySessions (LLM-642) ──────────────────────────────────────────────
// The conversation half of /agent/memory/sync: which of the client's local
// sessions to upload. The rows come from a query that deliberately includes
// soft-deleted and tombstoned notes, so "the server has a row" means "do not
// re-upload" — the contract that makes nightly retention stick.

const { classifySessions } = agentRouter;

function serverRows(entries) {
    return entries.map(([id, size]) => ({ session_id: id, file_size: String(size) }));
}

const A = 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa';
const B = 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb';

test('classifySessions: a session the server has no row for is missing', () => {
    const { missing, stale } = classifySessions([{ id: A, file_size: 100 }], [], true);
    assert.deepEqual(missing, [A]);
    assert.deepEqual(stale, []);
});

test('classifySessions: any row at all — live, soft-deleted or tombstoned — makes a session present', () => {
    // A retired conversation must not come back as "missing": that is the
    // re-upload loop that filled the quota with copies. The row the query
    // returns for it carries the size it was last uploaded at.
    const { missing, stale } = classifySessions([{ id: A, file_size: 100 }], serverRows([[A, 100]]), true);
    assert.deepEqual(missing, []);
    assert.deepEqual(stale, []);
});

test('classifySessions: stale only when the local file outgrew the largest size the server holds', () => {
    const items = [{ id: A, file_size: 150 }, { id: B, file_size: 90 }];
    const { missing, stale } = classifySessions(items, serverRows([[A, 100], [B, 100]]), true);
    assert.deepEqual(missing, []);
    // A grew (new material to fetch); B shrank or was rewritten — not stale.
    assert.deepEqual(stale, [A]);
});

test('classifySessions: legacy id-only clients are never stale', () => {
    const { missing, stale } = classifySessions([{ id: A, file_size: 0 }], serverRows([[A, 100]]), false);
    assert.deepEqual(missing, []);
    assert.deepEqual(stale, []);
});

test('classifySessions: server session ids match case-insensitively', () => {
    const { missing } = classifySessions([{ id: A, file_size: 1 }], serverRows([[A.toUpperCase(), 1]]), true);
    assert.deepEqual(missing, []);
});

test('classifySessions: a client id in upper case still matches its server row', () => {
    // The route lowercases ids before classifying; the function must not
    // depend on it.
    const { missing, stale } = classifySessions([{ id: A.toUpperCase(), file_size: 1 }], serverRows([[A, 1]]), true);
    assert.deepEqual(missing, []);
    assert.deepEqual(stale, []);
});
