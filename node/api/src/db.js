const { Pool, types } = require('pg');

const pool = new Pool({
    connectionString: process.env.DATABASE_URL
});

// Parse pgvector text representations into JS values.
// vector/halfvec: "[1,2,3]" → [1, 2, 3]
function parseVector(value) {
    if (value === null) return null;
    return value.substring(1, value.length - 1).split(',').map(Number);
}

// Register pgvector type parsers globally via pg.types so every client
// gets them automatically — no per-connection handler needed, no race.
// Must be called once before any queries that return vector columns.
async function init() {
    const client = await pool.connect();
    try {
        const result = await client.query(
            "SELECT typname, oid FROM pg_type WHERE typname IN ('vector', 'halfvec', 'sparsevec')"
        );
        for (const row of result.rows) {
            if (row.typname === 'vector' || row.typname === 'halfvec') {
                types.setTypeParser(row.oid, parseVector);
            } else if (row.typname === 'sparsevec') {
                // sparsevec is not used in this codebase, skip for now
            }
        }
    } finally {
        client.release();
    }
}

pool.init = init;
module.exports = pool;
