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
    }

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
