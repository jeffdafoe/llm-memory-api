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
const { EventEmitter } = require('node:events');

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
const UNLOCK_FAILURE = 'simulated pg_advisory_unlock failure';
const ACQUIRE_FAILURE = 'simulated pg_try_advisory_lock failure';
const CONNECT_FAILURE = 'simulated pool.connect failure';
const RUN_FAILURE = 'simulated agent-list query failure';

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

// Stand-in for the client runDream checks out to hold the advisory run lock
// (LLM-532). It answers the two lock statements itself and delegates anything
// else to the per-test pool.query stub, so a stray query on the lock client
// still hits that stub's strict unmatched-SQL guard. `granted` false simulates
// a second run finding the lock already held.
//
// Every checkout, lock statement and release is recorded in order, which is
// what lets the tests below assert the ordering that actually matters: the lock
// is taken before any run query and released only after the last one.
// `acquireFails` makes the pg_try_advisory_lock statement throw (an unusable
// connection); `unlockHeld` false makes pg_advisory_unlock report that this
// session did not hold the lock. The client is a real EventEmitter so a test
// can emit 'error' on it mid-run, which is how a dropped connection — and
// therefore a silently released lock — reaches the code under test.
function makeLockStub({ granted = true, unlockFails = false, acquireFails = false, unlockHeld = true } = {}) {
    const events = [];
    const client = new EventEmitter();
    client.query = async (sql, params) => {
        if (sql.includes('pg_try_advisory_lock')) {
            if (acquireFails) {
                events.push({ kind: 'acquire-failed' });
                throw new Error(ACQUIRE_FAILURE);
            }
            events.push({ kind: 'acquire', key: params[0], granted });
            return { rows: [{ ok: granted }] };
        }
        if (sql.includes('pg_advisory_unlock')) {
            events.push({ kind: 'unlock', key: params[0] });
            if (unlockFails) {
                throw new Error(UNLOCK_FAILURE);
            }
            return { rows: [{ ok: unlockHeld }] };
        }
        return pool.query(sql, params);
    };
    // release(err) destroys the connection rather than returning it to the
    // pool; the argument is recorded so the failure paths are checkable.
    client.release = (err) => {
        events.push({ kind: 'release', destroyed: Boolean(err) });
    };
    return {
        events,
        client,
        // Simulate the connection dropping mid-run. pg's own Pool handler is
        // what destroys the client in production; here the only listener that
        // matters is the one the lock installs.
        killConnection: () => {
            client.emit('error', new Error('simulated connection loss'));
        },
        listenerCount: () => client.listenerCount('error'),
        connect: async () => {
            events.push({ kind: 'connect' });
            return client;
        },
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
    // The default lock stub: granted. A test that needs the contended path
    // reassigns this and pool.connect before calling runDream.
    let lockStub;

    let originalQuery;
    let originalConnect;
    let originalConfigGet;
    let originalCronSchedule;
    let originalCronValidate;
    let originalConsoleLog;
    let originalConsoleError;

    beforeEach(() => {
        originalQuery = pool.query;
        originalConnect = pool.connect;
        originalConfigGet = config.get;
        originalCronSchedule = cron.schedule;
        originalCronValidate = cron.validate;
        originalConsoleLog = console.log;
        originalConsoleError = console.error;
        dreamEvents = [];
        consoleErrors = [];
        // Without this every test would try to open a real connection for the
        // run lock, since runDream now takes it before doing anything else.
        lockStub = makeLockStub();
        pool.connect = lockStub.connect;
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
        pool.connect = originalConnect;
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

    // --- LLM-532: the advisory run lock ---------------------------------
    //
    // What the lock buys is negative: a second concurrent run must not read a
    // cursor, plan a window, or stamp progress. So these assert on what does
    // NOT happen (no query at all on the contended path) as much as on the
    // returned summary.

    test('a second run finds the lock held and returns without touching anything', async () => {
        lockStub = makeLockStub({ granted: false });
        pool.connect = lockStub.connect;
        // Any query at all would mean the guard let the run start.
        pool.query = async (sql) => {
            throw new Error('no query should run while the lock is held: ' + sql.trim().slice(0, 60));
        };

        const result = await runDream();

        assert.deepEqual(result, { skipped: true, reason: 'already running' });

        // Nothing to unlock when the lock was never granted — but the checked-out
        // connection still goes back to the pool intact, not destroyed.
        assert.deepEqual(
            lockStub.events.map(e => e.kind),
            ['connect', 'acquire', 'release']
        );
        assert.equal(lockStub.events[1].granted, false);
        assert.equal(lockStub.events[2].destroyed, false);
        // No unlock: unlocking a lock this session never held would be a no-op
        // at best, and at worst hides the contention.
        assert.equal(lockStub.events.some(e => e.kind === 'unlock'), false);
        // The listener is gone, so a pooled connection doesn't accumulate one
        // per contended run.
        assert.equal(lockStub.listenerCount(), 0);

        const skip = dreamEvents.find(e => e.action === 'skip');
        assert.ok(skip, 'no skip event emitted');
        assert.equal(skip.details.reason, 'another dream run holds the lock');
        // A contended run is not a failed run: nothing lands in error_log.
        assert.equal(dreamEvents.some(e => e.action === 'error'), false);
    });

    test('the lock is taken before any run query and released only after the last one', async () => {
        const stub = makeQueryStub({
            agentRows: [sharedAgentRow()],
            rosterRows: [rosterRow('constance-scott/', 'Constance Scott', midnightAlignedSince(1))],
            chunkResponder: () => [],
        });
        // Fold the run's queries into the lock timeline so the ordering the
        // guard depends on — lock held across the WHOLE run, not just its first
        // statement — is directly observable.
        pool.query = async (sql, params) => {
            lockStub.events.push({ kind: 'query' });
            return stub.query(sql, params);
        };

        const result = await runDream();
        assert.equal(result.failedSharedActorCount, 0);
        assert.ok(stub.cursorUpdates.length > 0, 'the run did no work to bracket');

        const kinds = lockStub.events.map(e => e.kind);
        assert.deepEqual(kinds.slice(0, 2), ['connect', 'acquire']);
        assert.deepEqual(kinds.slice(-2), ['unlock', 'release']);
        assert.ok(
            kinds.indexOf('query') > kinds.indexOf('acquire'),
            'a run query ran before the lock was acquired'
        );
        assert.ok(
            kinds.lastIndexOf('query') < kinds.indexOf('unlock'),
            'a run query ran after the lock was released'
        );

        // Acquire and release must name the same key, or the unlock is a no-op
        // and the lock leaks for the life of the connection.
        const acquire = lockStub.events.find(e => e.kind === 'acquire');
        const unlock = lockStub.events.find(e => e.kind === 'unlock');
        assert.equal(acquire.key, unlock.key);
        assert.equal(lockStub.events.find(e => e.kind === 'release').destroyed, false);
    });

    test('a run that throws still releases the lock', async () => {
        const stub = makeQueryStub({
            agentRows: [],
            rosterRows: [],
            chunkResponder: () => [],
        });
        pool.query = async (sql, params) => {
            if (sql.includes('FROM agent_configuration agc')) {
                throw new Error(RUN_FAILURE);
            }
            return stub.query(sql, params);
        };

        await assert.rejects(runDream, new RegExp(RUN_FAILURE));

        // Released in a finally, so the next run — the 04:00 cron after a failed
        // manual one — is not locked out.
        assert.deepEqual(
            lockStub.events.map(e => e.kind),
            ['connect', 'acquire', 'unlock', 'release']
        );
    });

    test('an unlock that fails destroys the connection rather than stranding the lock', async () => {
        lockStub = makeLockStub({ unlockFails: true });
        pool.connect = lockStub.connect;
        const stub = makeQueryStub({
            agentRows: [sharedAgentRow()],
            rosterRows: [],
            chunkResponder: () => [],
        });
        pool.query = stub.query;

        // The run's own result is unaffected — the unlock failure happens after
        // the work is done and must not turn a clean run into a rejection.
        const result = await runDream();
        assert.equal(result.failedSharedActorCount, 0);

        // Destroying the connection ends the session, and Postgres drops
        // session-scoped advisory locks with it.
        const release = lockStub.events.find(e => e.kind === 'release');
        assert.equal(release.destroyed, true);

        const failure = dreamEvents.find(e => e.action === 'lock-release-error');
        assert.ok(failure, 'no lock-release-error event emitted');
        assert.equal(failure.details.error, UNLOCK_FAILURE);
    });

    // The lock's failure mode that matters: if its Postgres session dies the
    // lock is released while the run is still going, and another run can start
    // on the same cursors. The run must stop rather than keep writing.
    test('a lock lost mid-run aborts the run before the next cursor write', async () => {
        const since = midnightAlignedSince(3);
        const stub = makeQueryStub({
            agentRows: [sharedAgentRow()],
            rosterRows: [rosterRow('constance-scott/', 'Constance Scott', since)],
            chunkResponder: () => [],
        });
        // Kill the connection after the first chunk's cursor advance. The lock
        // is checked at the top of each chunk, so the run should stop there —
        // with the completed chunk's cursor write kept and no further ones.
        pool.query = async (sql, params) => {
            const result = await stub.query(sql, params);
            if (sql.includes('UPDATE sim_shared_actor SET last_dream_at') && stub.cursorUpdates.length === 1) {
                lockStub.killConnection();
            }
            return result;
        };

        assert.ok(expectedChunkCount(since, Date.now()) > 1, 'need a multi-chunk window to observe the abort');
        await assert.rejects(runDream, /dream run lock lost/);

        // The whole point: no cursor advanced after the lock was gone.
        assert.equal(stub.cursorUpdates.length, 1);

        const lostEvent = dreamEvents.find(e => e.action === 'lock-connection-error');
        assert.ok(lostEvent, 'the connection loss was not logged');

        // Still released — the finally runs on the failure path too, so the
        // pooled connection is not leaked along with the run.
        assert.deepEqual(
            lockStub.events.map(e => e.kind),
            ['connect', 'acquire', 'unlock', 'release']
        );
        assert.equal(lockStub.listenerCount(), 0);
    });

    test('an unlock reporting the lock was not held is logged, not swallowed', async () => {
        lockStub = makeLockStub({ unlockHeld: false });
        pool.connect = lockStub.connect;
        const stub = makeQueryStub({
            agentRows: [sharedAgentRow()],
            rosterRows: [],
            chunkResponder: () => [],
        });
        pool.query = stub.query;

        await runDream();

        // ok=false means this session did not hold the lock when it unlocked —
        // i.e. it was lost at some point. Nothing to clean up, but it is the
        // only evidence of a window where a second run could have started.
        const notHeld = dreamEvents.find(e => e.action === 'lock-release-not-held');
        assert.ok(notHeld, 'a false pg_advisory_unlock result was ignored');
    });

    test('a connection that cannot be checked out fails the run without any query', async () => {
        pool.connect = async () => {
            throw new Error(CONNECT_FAILURE);
        };
        pool.query = async () => {
            throw new Error('no query should run when the lock connection is unavailable');
        };

        await assert.rejects(runDream, new RegExp(CONNECT_FAILURE));
    });

    test('a failed lock acquisition destroys the connection and fails the run', async () => {
        lockStub = makeLockStub({ acquireFails: true });
        pool.connect = lockStub.connect;
        pool.query = async () => {
            throw new Error('no query should run when the lock could not be acquired');
        };

        await assert.rejects(runDream, new RegExp(ACQUIRE_FAILURE));

        // A connection whose lock statement failed is suspect: returning it to
        // the pool intact would hand the next borrower a broken session.
        assert.deepEqual(
            lockStub.events.map(e => e.kind),
            ['connect', 'acquire-failed', 'release']
        );
        assert.equal(lockStub.events[2].destroyed, true);
        assert.equal(lockStub.listenerCount(), 0);
    });

    test('a disabled run never checks out a connection for the lock', async () => {
        config.get = (key) => {
            if (key === 'dream_processing_enabled') {
                return 'false';
            }
            throw new Error('unexpected config key in test: ' + key);
        };
        pool.query = async () => {
            throw new Error('no query should run when dream processing is disabled');
        };

        const result = await runDream();

        assert.deepEqual(result, { skipped: true, reason: 'disabled' });
        assert.deepEqual(lockStub.events, []);
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
