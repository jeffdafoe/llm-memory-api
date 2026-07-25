// Integration tests for the shared-VA half of the dream cron (LLM-521,
// follow-up to LLM-519). These drive the REAL runDream() ->
// processSharedAgent -> dreamSharedRoster -> runChunkLoop call chain and
// assert the failure accounting the cron's monitoring depends on:
//
//   1. a failed chunk surfaces as failedSharedActorCount > 0
//   2. plannedChunks / completedChunks are reported separately and accurately
//   3. the scheduler's completion event carries status 'completed-with-errors'
//
// Run with: npm test (from node/api).
//
// How this runs without a database or a model provider: the only collaborators
// on this path are `pool` (the pg Pool instance) and `config` (a module object),
// both of which are plain objects in the require cache and so can be swapped
// per-test. dream.js's destructured requires (invokeAgent, saveNote, readNote)
// are deliberately never reached — a chunk that FAILS fails at the
// conversation-log query, and a chunk that COMPLETES here is the "no logs"
// early return. Both happen before the model call, so the accounting surface is
// exercised end-to-end while the note-writing half stays out of scope. Covering
// that half needs a mockable module boundary (see LLM-521's options 1 and 2).
//
// findDreamAgent is NOT stubbed: its expertise lookup is answered with rows
// created_by the system actor, so the real isTrustedCreator check passes.

const { describe, test, beforeEach, afterEach } = require('node:test');
const assert = require('node:assert/strict');

const pool = require('../db');
const config = require('./config');
const cron = require('node-cron');
const actors = require('./actors');
const adminPermissions = require('./admin-permissions');
const { runDream, startDreamScheduler } = require('./dream');

const DAY_MS = 24 * 60 * 60 * 1000;
const SYSTEM_ACTOR_ID = 1;
const POOLED_ACTOR_ID = 42;
const POOLED_AGENT = 'salem-vendor';
const CHUNK_FAILURE = 'simulated conversation-log query failure';

// Midnight-aligned on purpose: computeDailyChunks splits [since, now] on
// UTC-day boundaries, so an aligned `since` makes the planned chunk count
// exactly ceil((now - since) / one day) — which the tests below re-derive to
// pin plannedChunks without hard-coding a number.
function midnightAlignedSince(daysBack) {
    const now = new Date();
    const todayStart = Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate());
    return new Date(todayStart - daysBack * DAY_MS);
}

function expectedChunkCount(since, nowMs) {
    return Math.ceil((nowMs - since.getTime()) / DAY_MS);
}

// Yield until `predicate` holds or the tick budget runs out. Used to observe
// logError's fire-and-forget error_log insert, which is deliberately not
// awaited by the code under test (see the persistence test below).
async function waitFor(predicate, ticks = 50) {
    for (let i = 0; i < ticks; i++) {
        if (predicate()) return true;
        await new Promise(resolve => setImmediate(resolve));
    }
    return predicate();
}

const CONFIG_VALUES = {
    dream_processing_enabled: 'true',
    // The cron paces itself between agents, villagers and chunks; this suite has
    // nothing to be polite to. 1ms rather than 0 because both call sites read
    // `parseInt(...) || <default>`, so a zero falls through to the real delay.
    dream_interagent_delay: '1',
    dream_interchunk_delay: '1',
    dream_cron_schedule: '0 4 * * *',
};

// One shared-VA villager roster row. `lastDreamAt` set means the first-run
// backfill branch (MIN(created_at) lookup) is skipped, so the planned window is
// exactly the cursor-to-now span.
function rosterRow(slugPrefix, displayName, lastDreamAt) {
    return { slug_prefix: slugPrefix, display_name: displayName, last_dream_at: lastDreamAt };
}

// A sim-shared agent row as runDream's agent query returns it.
function sharedAgentRow() {
    return {
        name: POOLED_AGENT,
        actor_id: POOLED_ACTOR_ID,
        dream_mode: 'sim-shared',
        dream_source: 'conversation',
        last_dream_at: null,
        startup_instructions: null,
    };
}

// Build the pool.query stand-in. `chunkResponder` is called once per
// conversation-log query (one per chunk attempt, in order) and returns the rows
// that query should yield — or throws to simulate a failing chunk.
// `errorLogResponder`, when given, decides what the error_log INSERT does.
//
// Returns the query function plus two recordings: the cursor advances (evidence
// that runChunkLoop stamped progress only for chunks that actually succeeded)
// and the error_log inserts (the cron's durable monitoring record).
//
// Every branch matches on SQL text, which is brittle against reformatting of the
// queries in dream.js — deliberately so: an unmatched query THROWS rather than
// returning empty rows, which turns a production query change into a loud test
// failure instead of a silently wrong result.
function makeQueryStub({ agentRows, rosterRows, chunkResponder, errorLogResponder }) {
    const cursorUpdates = [];
    const errorLogInserts = [];
    let chunkQueryCount = 0;

    async function query(sql, params) {
        // isTrustedCreator's system-actor lookup. Must precede the general
        // actors-by-name branch below, whose pattern this SQL also matches.
        if (sql.includes("FROM actors WHERE name = 'system'")) {
            return { rows: [{ id: SYSTEM_ACTOR_ID }] };
        }
        // findDreamAgent: every dream-* expertise tag resolves to a configured
        // agent owned by the system actor.
        if (sql.includes('ac.expertise @> jsonb_build_array')) {
            return {
                rows: [{
                    id: 900,
                    name: params[0] + '-va',
                    created_by: SYSTEM_ACTOR_ID,
                    provider: 'openrouter',
                    model: 'test-model',
                    api_key: 'test-key',
                }],
            };
        }
        // runDream's dream-enabled agent list.
        if (sql.includes('FROM agent_configuration agc')) {
            return { rows: agentRows };
        }
        if (sql.includes('FROM sim_shared_actor')) {
            return { rows: rosterRows };
        }
        if (sql.includes('UPDATE sim_shared_actor SET last_dream_at')) {
            cursorUpdates.push({ to: params[0], actorId: params[1], prefix: params[2] });
            return { rows: [], rowCount: 1 };
        }
        // The per-chunk conversation-log query — the one surface each test
        // scripts to decide which chunks succeed and which blow up.
        if (sql.includes('FROM documents') && sql.includes('created_at >')) {
            // Count the attempt before dispatching: a responder that throws must
            // still advance the index, or the next villager's first chunk would
            // replay the failing script.
            const index = chunkQueryCount;
            chunkQueryCount++;
            return { rows: chunkResponder(index, params) };
        }
        if (sql.includes('INSERT INTO error_log')) {
            errorLogInserts.push({
                subsystem: params[0],
                action: params[1],
                actorId: params[2],
                context: params[3],
                message: params[5],
            });
            if (errorLogResponder) {
                return errorLogResponder();
            }
            return { rows: [], rowCount: 1 };
        }
        // logError resolves an agent name to an actor id before inserting.
        if (sql.includes('FROM actors WHERE name')) {
            return { rows: [] };
        }
        throw new Error('unexpected query in test: ' + sql.trim().slice(0, 120));
    }

    return {
        query,
        cursorUpdates,
        errorLogInserts,
        chunkQueryCount: () => chunkQueryCount,
    };
}

// concurrency: 1 is explicit rather than incidental. Every test here swaps
// process-global state (pool.query, config.get, console.log, cron.schedule), so
// two of them running at once would corrupt each other. node:test currently
// serializes a file's tests by default; pinning it means a future default can't
// quietly break the suite.
describe('shared-VA dream cron', { concurrency: 1 }, () => {
    // dream.js logs through logger.log, a destructured binding it cannot be
    // swapped out of, so console.log is the observable seam for the cron's event
    // stream. The recorder is installed for every test — it makes the emitted
    // events assertable and keeps the suite's output readable (the cron is
    // chatty). console.error is recorded too: it is where logError's
    // fire-and-forget insert reports its own failure.
    let dreamEvents = [];
    let consoleErrors = [];

    let originalQuery;
    let originalConfigGet;
    let originalCronSchedule;
    let originalCronValidate;
    let originalConsoleLog;
    let originalConsoleError;

    beforeEach(() => {
        originalQuery = pool.query;
        originalConfigGet = config.get;
        originalCronSchedule = cron.schedule;
        originalCronValidate = cron.validate;
        originalConsoleLog = console.log;
        originalConsoleError = console.error;
        dreamEvents = [];
        consoleErrors = [];
        // Both services cache by actor across calls; a stale entry from a
        // previous test would let a later test pass without its stub consulted.
        actors.clearCache();
        adminPermissions.clearCache();
        config.get = (key) => {
            if (!(key in CONFIG_VALUES)) {
                throw new Error('unexpected config key in test: ' + key);
            }
            return CONFIG_VALUES[key];
        };
        console.log = (prefix, payload) => {
            const match = typeof prefix === 'string' && prefix.match(/^\[dream\] \S+ (.+):$/);
            if (match) {
                dreamEvents.push({ action: match[1], details: JSON.parse(payload) });
            }
        };
        console.error = (...args) => {
            consoleErrors.push(args.join(' '));
        };
    });

    afterEach(() => {
        pool.query = originalQuery;
        config.get = originalConfigGet;
        cron.schedule = originalCronSchedule;
        cron.validate = originalCronValidate;
        console.log = originalConsoleLog;
        console.error = originalConsoleError;
    });

    // Install the scheduler with node-cron stubbed out, and return the callback
    // it registered so a "cron firing" can be driven synchronously.
    function captureScheduledJob() {
        let job = null;
        cron.validate = () => true;
        cron.schedule = (schedule, callback) => {
            job = callback;
            return { stop: () => {} };
        };
        startDreamScheduler();
        assert.ok(job, 'scheduler did not register a job');
        return job;
    }

    test('a failed chunk is counted as a failed shared actor and stops that villager', async () => {
        const since = midnightAlignedSince(1);
        const stub = makeQueryStub({
            agentRows: [sharedAgentRow()],
            rosterRows: [rosterRow('constance-scott/', 'Constance Scott', since)],
            // First chunk: no conversation notes in the window — a legitimate
            // skip, and a completion. Second: the query itself fails.
            chunkResponder: (index) => {
                if (index === 0) return [];
                throw new Error(CHUNK_FAILURE);
            },
        });
        pool.query = stub.query;

        const plannedBefore = expectedChunkCount(since, Date.now());
        const result = await runDream();
        const plannedAfter = expectedChunkCount(since, Date.now());

        assert.equal(result.failedSharedActorCount, 1);

        const shared = result.results.find(r => r.mode === 'sim-shared');
        assert.equal(shared.agent, POOLED_AGENT);
        assert.equal(shared.actorCount, 1);
        assert.equal(shared.failedActorCount, 1);
        assert.equal(shared.error, undefined);

        const actor = shared.actors[0];
        assert.equal(actor.prefix, 'constance-scott/');
        assert.equal(actor.completedChunks, 1);
        // plannedChunks counts the whole window, not just the chunks attempted —
        // that gap is the point of reporting the two separately.
        assert.ok(
            actor.plannedChunks >= plannedBefore && actor.plannedChunks <= plannedAfter,
            `plannedChunks ${actor.plannedChunks} outside expected ${plannedBefore}..${plannedAfter}`
        );
        assert.ok(actor.plannedChunks > actor.completedChunks);

        // runChunkLoop stops this villager at the first failed chunk so the next
        // cron retries it rather than skipping past unprocessed logs.
        assert.equal(actor.chunks.length, 2);
        assert.equal(actor.chunks[0].skipped, true);
        assert.equal(actor.chunks[0].reason, 'no logs');
        assert.equal(actor.chunks[1].error, CHUNK_FAILURE);

        // The cursor advanced for the completed chunk only — a failed chunk must
        // not stamp progress over logs it never consumed.
        assert.equal(stub.cursorUpdates.length, 1);
        assert.equal(stub.cursorUpdates[0].actorId, POOLED_ACTOR_ID);
        assert.equal(stub.cursorUpdates[0].prefix, 'constance-scott/');
    });

    test('one failing villager does not fail the rest of the roster', async () => {
        const since = midnightAlignedSince(1);
        // Roster order matches the query's ORDER BY slug_prefix ASC.
        const stub = makeQueryStub({
            agentRows: [sharedAgentRow()],
            rosterRows: [
                rosterRow('constance-scott/', 'Constance Scott', since),
                rosterRow('josiah-thorne/', 'Josiah Thorne', since),
            ],
            // Constance's first chunk fails immediately; every later query
            // (Josiah's chunks) returns an empty, cleanly skipped window.
            chunkResponder: (index) => {
                if (index === 0) throw new Error(CHUNK_FAILURE);
                return [];
            },
        });
        pool.query = stub.query;

        const plannedBefore = expectedChunkCount(since, Date.now());
        const result = await runDream();
        const plannedAfter = expectedChunkCount(since, Date.now());

        // Actor granularity: two villagers processed, one of them failed.
        const shared = result.results.find(r => r.mode === 'sim-shared');
        assert.equal(shared.actorCount, 2);
        assert.equal(shared.failedActorCount, 1);
        assert.equal(result.failedSharedActorCount, 1);

        const [constance, josiah] = shared.actors;
        assert.equal(constance.prefix, 'constance-scott/');
        assert.equal(constance.completedChunks, 0);
        assert.equal(constance.chunks.length, 1);
        assert.equal(constance.chunks[0].error, CHUNK_FAILURE);

        assert.equal(josiah.prefix, 'josiah-thorne/');
        assert.equal(josiah.completedChunks, josiah.plannedChunks);
        assert.ok(
            josiah.plannedChunks >= plannedBefore && josiah.plannedChunks <= plannedAfter,
            `plannedChunks ${josiah.plannedChunks} outside expected ${plannedBefore}..${plannedAfter}`
        );
        assert.ok(josiah.chunks.every(c => !c.error));

        // Cursor advances: none for Constance (her only chunk failed), one per
        // completed chunk for Josiah.
        assert.equal(stub.cursorUpdates.length, josiah.plannedChunks);
        assert.ok(stub.cursorUpdates.every(u => u.prefix === 'josiah-thorne/'));
    });

    test("the scheduler reports a shared-actor failure as status 'completed-with-errors'", async () => {
        const since = midnightAlignedSince(1);
        const stub = makeQueryStub({
            agentRows: [sharedAgentRow()],
            rosterRows: [rosterRow('constance-scott/', 'Constance Scott', since)],
            chunkResponder: () => {
                throw new Error(CHUNK_FAILURE);
            },
        });
        pool.query = stub.query;

        const job = captureScheduledJob();
        await job();

        const complete = dreamEvents.find(e => e.action === 'cron-complete');
        assert.ok(complete, 'no cron-complete event emitted');
        assert.equal(complete.details.status, 'completed-with-errors');
        assert.equal(complete.details.result.failedSharedActorCount, 1);

        const failureRecord = dreamEvents.find(e => e.action === 'cron-shared-actor-failures');
        assert.ok(failureRecord, 'no cron-shared-actor-failures event emitted');
        assert.match(failureRecord.details.error, /^1 shared-VA actor\(s\) failed/);
    });

    // The completion event's `status` field is the authoritative failure signal
    // by design (LLM-519): a consumer reading it never has to race it against a
    // separate record. The error_log row is monitoring, written through
    // logger.logError's deliberately fire-and-forget insert — so this asserts the
    // row is written with the right payload, NOT that it lands before
    // cron-complete. Asserting that ordering would encode a guarantee the design
    // explicitly disclaims, and would make the test fail the moment the insert is
    // (correctly) allowed to lag.
    test('a failing run writes its monitoring row to error_log', async () => {
        const stub = makeQueryStub({
            agentRows: [sharedAgentRow()],
            rosterRows: [rosterRow('constance-scott/', 'Constance Scott', midnightAlignedSince(1))],
            chunkResponder: () => {
                throw new Error(CHUNK_FAILURE);
            },
        });
        pool.query = stub.query;

        const job = captureScheduledJob();
        await job();
        // Wait for BOTH rows independently. The two logError calls are separately
        // fire-and-forget and take different numbers of hops to reach the insert
        // (the chunk one resolves an agent name first), so neither is guaranteed
        // to become observable before the other.
        const observed = await waitFor(() =>
            stub.errorLogInserts.some(i => i.action === 'cron-shared-actor-failures')
            && stub.errorLogInserts.some(i => i.action === 'chunk-error')
        );
        assert.equal(observed, true, 'expected both monitoring rows to reach error_log');

        const cronRecord = stub.errorLogInserts.find(i => i.action === 'cron-shared-actor-failures');
        assert.equal(cronRecord.subsystem, 'dream');
        assert.match(cronRecord.message, /^1 shared-VA actor\(s\) failed/);

        // The chunk that caused it is recorded separately, so monitoring can see
        // which villager failed and not just that the run did.
        const chunkRecord = stub.errorLogInserts.find(i => i.action === 'chunk-error');
        assert.equal(chunkRecord.message, CHUNK_FAILURE);
    });

    // The other half of that decision: monitoring must never be able to take the
    // run down with it. A rejecting error_log insert is swallowed by logError's
    // catch, the run still resolves, and the reported status is unchanged.
    test('an error_log write failure does not change the run status', async () => {
        const stub = makeQueryStub({
            agentRows: [sharedAgentRow()],
            rosterRows: [rosterRow('constance-scott/', 'Constance Scott', midnightAlignedSince(1))],
            chunkResponder: () => {
                throw new Error(CHUNK_FAILURE);
            },
            errorLogResponder: () => {
                throw new Error('simulated error_log insert failure');
            },
        });
        pool.query = stub.query;

        const job = captureScheduledJob();
        await job();
        await waitFor(() => consoleErrors.length > 0);

        const complete = dreamEvents.find(e => e.action === 'cron-complete');
        assert.ok(complete, 'no cron-complete event emitted');
        assert.equal(complete.details.status, 'completed-with-errors');
        assert.equal(complete.details.result.failedSharedActorCount, 1);
        assert.ok(
            consoleErrors.some(line => line.includes('Failed to write to error_log table')),
            'the error_log write failure was not swallowed and reported'
        );
    });

    test("a clean shared-VA run reports status 'ok'", async () => {
        const stub = makeQueryStub({
            agentRows: [sharedAgentRow()],
            rosterRows: [rosterRow('constance-scott/', 'Constance Scott', midnightAlignedSince(1))],
            chunkResponder: () => [],
        });
        pool.query = stub.query;

        const job = captureScheduledJob();
        await job();

        const complete = dreamEvents.find(e => e.action === 'cron-complete');
        assert.ok(complete, 'no cron-complete event emitted');
        assert.equal(complete.details.status, 'ok');
        assert.equal(complete.details.result.failedSharedActorCount, 0);
        assert.equal(dreamEvents.some(e => e.action === 'cron-shared-actor-failures'), false);
        assert.equal(stub.errorLogInserts.length, 0);
    });

    test('an empty roster is a clean run, not a failure', async () => {
        const stub = makeQueryStub({
            agentRows: [sharedAgentRow()],
            rosterRows: [],
            chunkResponder: () => {
                throw new Error('no chunk query should run for an empty roster');
            },
        });
        pool.query = stub.query;

        const result = await runDream();

        assert.equal(result.failedSharedActorCount, 0);
        const shared = result.results.find(r => r.mode === 'sim-shared');
        assert.equal(shared.actorCount, 0);
        assert.equal(shared.failedActorCount, undefined);
        assert.equal(stub.chunkQueryCount(), 0);
    });

    test('a roster row with an unusable slug prefix fails only that villager', async () => {
        const stub = makeQueryStub({
            agentRows: [sharedAgentRow()],
            rosterRows: [
                // Not canonical (missing trailing slash) — the distiller never
                // writes this, so it means a hand-edited or corrupt row.
                rosterRow('constance-scott', 'Constance Scott', midnightAlignedSince(1)),
                rosterRow('josiah-thorne/', 'Josiah Thorne', midnightAlignedSince(1)),
            ],
            chunkResponder: () => [],
        });
        pool.query = stub.query;

        const result = await runDream();

        const shared = result.results.find(r => r.mode === 'sim-shared');
        assert.equal(shared.actorCount, 2);
        assert.equal(shared.failedActorCount, 1);
        assert.equal(result.failedSharedActorCount, 1);
        assert.equal(shared.actors[0].error, 'invalid slug prefix');
        // The valid villager still dreamed.
        assert.equal(shared.actors[1].prefix, 'josiah-thorne/');
        assert.ok(shared.actors[1].completedChunks > 0);
    });

    test('a sim-shared agent misconfigured to notes source is rejected without dreaming', async () => {
        const agentRow = sharedAgentRow();
        agentRow.dream_source = 'notes';
        const stub = makeQueryStub({
            agentRows: [agentRow],
            rosterRows: [rosterRow('constance-scott/', 'Constance Scott', midnightAlignedSince(1))],
            chunkResponder: () => {
                throw new Error('no chunk query should run for a rejected agent');
            },
        });
        pool.query = stub.query;

        const result = await runDream();

        assert.equal(result.failedSharedActorCount, 1);
        const shared = result.results.find(r => r.mode === 'sim-shared');
        assert.equal(shared.failedActorCount, 1);
        assert.match(shared.error, /dream_source must be conversation/);
        // The roster is never even read — the whole agent is rejected up front so
        // a notes-mode read can't write cross-villager material into one subtree.
        assert.equal(stub.chunkQueryCount(), 0);
    });
});
