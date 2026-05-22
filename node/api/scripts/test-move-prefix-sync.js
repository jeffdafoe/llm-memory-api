#!/usr/bin/env node
// Regression test for movePrefix's conflict-skip sync-mapping fix.
//
// Bug: when a prefix move SKIPS a conflicting destination (the caller did not
// choose to overwrite it), the source note stays at its old slug — but the
// note_synchronization rewrite used to move that note's sync mapping to the new
// prefix anyway, leaving the mapping pointing at a slug with no document behind
// it (a silent persistent-state inconsistency). The documents + memory_chunks
// rewrites correctly excluded skipped source slugs; the note_synchronization
// rewrite did not.
//
// The bug lives in a SQL WHERE clause, so we reproduce it at the query level: a
// require('../db') stub records every query movePrefix issues, and we assert the
// note_synchronization UPDATE carries the same skip exclusion (and the skipped
// source slug as its $5 param) that the other two rewrites have.
//
// No real DB, no network. Run: node scripts/test-move-prefix-sync.js
// Exits 0 on pass, 1 on any failure.

const Module = require('module');
const origRequire = Module.prototype.require;

// Recorded client.query calls from inside the transaction body.
let clientQueries = [];
// Rows the stub serves to movePrefix's two pool.query reads (source + conflicts).
let sourceRows = [];
let conflictRows = [];

// Stub ../db BEFORE requiring the service (documents.js binds the pool at load).
Module.prototype.require = function(id) {
    if (id === '../db') {
        return {
            // pool.query — the sourceNotes count and the conflicts join.
            query: async (sql) => {
                if (/JOIN\s+documents\s+d2/i.test(sql)) {
                    return { rows: conflictRows, rowCount: conflictRows.length };
                }
                return { rows: sourceRows, rowCount: sourceRows.length };
            },
            // pool.connect — the transaction client; record every query it runs.
            connect: async () => ({
                query: async (sql, params) => {
                    clientQueries.push({ sql, params });
                    if (/UPDATE\s+documents/i.test(sql)) {
                        return { rowCount: sourceRows.length, rows: [] };
                    }
                    return { rowCount: 0, rows: [] };
                },
                release: () => {}
            })
        };
    }
    return origRequire.apply(this, arguments);
};

const { movePrefix } = require('../src/services/documents');

let passed = 0;
let failed = 0;
const failures = [];

function assert(label, cond, detail) {
    if (cond) {
        passed++;
    } else {
        failed++;
        failures.push(`  FAIL [${label}] ${detail || ''}`);
    }
}

// The note_synchronization UPDATE among the recorded transaction queries.
function syncUpdate() {
    return clientQueries.find(q => /UPDATE\s+note_synchronization/i.test(q.sql));
}

async function run() {
    // --- Scenario 1: a skipped conflict (overwriteSlugs empty) --------------
    // Source notes under tasks/x/ : A and B. Destination tasks/done/x/B already
    // exists → conflict. Caller does not overwrite it → B is skipped, so B's
    // document stays at tasks/x/B and its sync mapping MUST stay too.
    clientQueries = [];
    sourceRows = [{ slug: 'tasks/x/A', title: 'A' }, { slug: 'tasks/x/B', title: 'B' }];
    conflictRows = [{ slug: 'tasks/done/x/B', title: 'B (existing)' }];
    await movePrefix('home', 'tasks/x/', 'tasks/done/x/', { overwriteSlugs: [] });

    const u1 = syncUpdate();
    assert('skip: note_sync UPDATE issued', !!u1, 'no note_synchronization UPDATE was issued');
    if (u1) {
        assert('skip: note_sync UPDATE excludes skipped slugs (!= ALL($5))',
            /!=\s*ALL\(\$5\)/i.test(u1.sql),
            `sync UPDATE lacks the skip exclusion: ${u1.sql.replace(/\s+/g, ' ').trim()}`);
        const skipParam = Array.isArray(u1.params) ? u1.params[4] : undefined;
        assert('skip: skipped source slug passed to note_sync UPDATE ($5)',
            Array.isArray(skipParam) && skipParam.includes('tasks/x/B'),
            `expected $5 to include 'tasks/x/B', got ${JSON.stringify(skipParam)}`);
    }

    // --- Scenario 2: no conflicts → unconditional rewrite, no skip param ----
    clientQueries = [];
    sourceRows = [{ slug: 'tasks/y/A', title: 'A' }];
    conflictRows = [];
    await movePrefix('home', 'tasks/y/', 'tasks/done/y/', { overwriteSlugs: [] });

    const u2 = syncUpdate();
    assert('no-conflict: note_sync UPDATE issued', !!u2, 'no note_synchronization UPDATE was issued');
    if (u2) {
        assert('no-conflict: note_sync UPDATE has no skip exclusion',
            !/!=\s*ALL/i.test(u2.sql),
            `unexpected skip exclusion when nothing was skipped: ${u2.sql.replace(/\s+/g, ' ').trim()}`);
        assert('no-conflict: note_sync UPDATE still carries the overwrite collision guard',
            /NOT\s+EXISTS/i.test(u2.sql),
            `sync UPDATE lacks the NOT EXISTS collision guard: ${u2.sql.replace(/\s+/g, ' ').trim()}`);
    }

    // --- Scenario 3: an overwritten conflict (HOME-287) ---------------------
    // Source notes under tasks/z/ : A and B. Destination tasks/done/z/B already
    // exists and the caller chooses to overwrite it. B's source row is rewritten
    // onto the dest slug — if the same actor synced both the source and the
    // pre-existing dest, that rewrite would violate UNIQUE(actor_id, namespace,
    // slug). The sync UPDATE must carry a NOT EXISTS guard that leaves a colliding
    // source row at its old slug instead of erroring the whole move.
    clientQueries = [];
    sourceRows = [{ slug: 'tasks/z/A', title: 'A' }, { slug: 'tasks/z/B', title: 'B' }];
    conflictRows = [{ slug: 'tasks/done/z/B', title: 'B (existing)' }];
    await movePrefix('home', 'tasks/z/', 'tasks/done/z/', { overwriteSlugs: ['tasks/done/z/B'] });

    const u3 = syncUpdate();
    assert('overwrite: note_sync UPDATE issued', !!u3, 'no note_synchronization UPDATE was issued');
    if (u3) {
        assert('overwrite: note_sync UPDATE carries the NOT EXISTS collision guard',
            /NOT\s+EXISTS/i.test(u3.sql),
            `sync UPDATE lacks the NOT EXISTS collision guard: ${u3.sql.replace(/\s+/g, ' ').trim()}`);
        // The guard's correlated subquery must key on the same actor and the
        // rewritten dest slug — not a blanket exclusion that would also strip an
        // uninvolved actor's dest mapping.
        assert('overwrite: collision guard correlates on actor_id',
            /dst\.actor_id\s*=\s*src\.actor_id/i.test(u3.sql),
            `collision guard does not correlate on actor_id: ${u3.sql.replace(/\s+/g, ' ').trim()}`);
    }

    // --- Scenario 4: nested-prefix move is rejected up front (HOME-287) -----
    // Moving "a/" -> "a/sub/" double-moves source rows and would make the sync
    // collision guard silently skip rows. movePrefix must reject it before any
    // write, with a 400.
    clientQueries = [];
    sourceRows = [{ slug: 'a/x', title: 'X' }];
    conflictRows = [];
    let rejected = false;
    let rejectedStatus = undefined;
    try {
        await movePrefix('home', 'a/', 'a/sub/', { overwriteSlugs: [] });
    } catch (err) {
        rejected = true;
        rejectedStatus = err.statusCode;
    }
    assert('nested: move under self is rejected', rejected, 'nested prefix move was not rejected');
    assert('nested: rejection is a 400', rejectedStatus === 400, `expected statusCode 400, got ${rejectedStatus}`);
    assert('nested: no transaction queries issued before reject',
        clientQueries.length === 0,
        `expected no client queries, got ${clientQueries.length}`);

    console.log(`\n${passed} passed, ${failed} failed`);
    if (failed > 0) {
        console.log(failures.join('\n'));
        process.exit(1);
    }
    process.exit(0);
}

run().catch(err => {
    console.error(err);
    process.exit(1);
});
