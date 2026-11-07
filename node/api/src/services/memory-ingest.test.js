// Tests for ingestContent's transaction (LLM-919). Run with: node --test (from
// node/api).
//
// The db pool and the embeddings call are stubbed before memory.js is
// required, so nothing here touches Postgres or OpenAI. The lock itself, two
// ingests of one file at once against a real database, is covered for both
// servers by the Go contract test TestIngestConcurrentKeepsOneVersion
// (go/server/contract/ingest_race_test.go).

const { test, beforeEach } = require('node:test');
const assert = require('node:assert/strict');

const pool = require('../db');
const embeddings = require('./embeddings');

let queries = [];
let failOn = [];
let released = 0;

embeddings.embed = async (texts) => texts.map(() => [0.1, 0.2, 0.3]);
pool.connect = async () => ({
    query: async (sql, params) => {
        queries.push({ sql, params });
        const verb = sql.trim().split(/\s+/)[0];
        if (failOn.includes(verb)) {
            throw new Error('simulated ' + verb + ' failure');
        }
        return { rows: [], rowCount: 0 };
    },
    release: () => {
        released++;
    }
});

const { ingestContent } = require('./memory');

beforeEach(() => {
    queries = [];
    failOn = [];
    released = 0;
});

function verbs() {
    return queries.map((q) => q.sql.trim().split(/\s+/)[0]);
}

test('ingest takes the per-file lock before it deletes the old chunks', async () => {
    const result = await ingestContent('agent', 'Notes/Race.md', '# One\n\nfirst\n\n# Two\n\nsecond');
    assert.equal(result.chunks_created, 2);
    assert.deepEqual(verbs(), ['BEGIN', 'SELECT', 'DELETE', 'INSERT', 'INSERT', 'COMMIT']);
    assert.match(queries[1].sql, /pg_advisory_xact_lock\(\$1, hashtext\(\$2::text \|\| '\/' \|\| LOWER\(\$3::text\)\)\)/);
    assert.deepEqual(queries[1].params, [919001, 'agent', 'Notes/Race.md']);
    assert.equal(released, 1);
});

test('a failed lock rolls back without deleting anything', async () => {
    failOn = ['SELECT'];
    await assert.rejects(ingestContent('agent', 'race.md', '# One\n\nfirst'), /simulated SELECT failure/);
    assert.deepEqual(verbs(), ['BEGIN', 'SELECT', 'ROLLBACK']);
    assert.equal(released, 1);
});
