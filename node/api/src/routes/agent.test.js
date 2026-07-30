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

test('resolvePruneCutoff parses an ISO timestamp from the client', () => {
    const cutoff = resolvePruneCutoff('2026-07-30T11:59:00.000Z', NOW);
    assert.equal(cutoff, Date.parse('2026-07-30T11:59:00.000Z'));
});

test('resolvePruneCutoff clamps a client clock running ahead of the server', () => {
    // Unclamped, this cutoff would sit past every remote updated_at and make
    // the concurrency guard vacuously true.
    const cutoff = resolvePruneCutoff('2026-07-30T18:00:00.000Z', NOW);
    assert.equal(cutoff, NOW);
});

test('resolvePruneCutoff returns null for a missing or unparseable value', () => {
    assert.equal(resolvePruneCutoff(undefined, NOW), null);
    assert.equal(resolvePruneCutoff('', NOW), null);
    assert.equal(resolvePruneCutoff('not a date', NOW), null);
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
