const { Pool } = require('pg');
const pgvector = require('pgvector/pg');

const pool = new Pool({
    connectionString: process.env.DATABASE_URL
});

// Register pgvector types on every new connection.
// Pool.on('connect') doesn't await async handlers (EventEmitter is sync),
// so the registerTypes query races against the first real query. We solve
// this by eagerly registering on a throwaway client at startup (init()),
// then re-registering on each new connection for long-running pools where
// connections get recycled.
pool.on('connect', (client) => {
    pgvector.registerTypes(client).catch(() => {});
});

// Call once before any queries. Checks out a client, registers types,
// and releases it — guarantees the first real query never races.
async function init() {
    const client = await pool.connect();
    try {
        await pgvector.registerTypes(client);
    } finally {
        client.release();
    }
}

pool.init = init;
module.exports = pool;
