// Runs one pass of a Node server background job against DATABASE_URL, for
// the contract harness's job steps; main.go beside it does the same with
// the Go server's code. Run with node/api as the working directory:
//
//     node <repo>/go/server/contract/runjob/runjob.js cleanup|dream
//
// The job is the server's own code. node-cron's schedule() is replaced so it
// hands back the callback instead of arming a timer, and the callback (the
// same function the cron would call) runs once.

const path = require('path');

const api = path.resolve(__dirname, '../../../../node/api/src');

// Requests in flight: a saved note is indexed in the background, and its
// embeddings call leaves the pool idle while it waits on the fake.
let fetches = 0;
const realFetch = globalThis.fetch;
globalThis.fetch = async (...args) => {
    fetches++;
    try {
        return await realFetch(...args);
    } finally {
        fetches--;
    }
};

async function settle(pool) {
    // updateUsage, logError's insert, the call log and note indexing are
    // fire-and-forget: wait until the pool has had nothing checked out or
    // queued, and no request has been in flight, for a while.
    let quietSince = Date.now();
    const deadline = Date.now() + 10000;
    while (Date.now() < deadline) {
        const busy = fetches > 0 || pool.waitingCount > 0 || pool.totalCount !== pool.idleCount;
        if (busy) quietSince = Date.now();
        else if (Date.now() - quietSince >= 200) return;
        await new Promise((resolve) => setTimeout(resolve, 20));
    }
    throw new Error('background queries did not finish');
}

async function main() {
    const job = process.argv[2];
    if (process.argv.length !== 3) throw new Error('usage: runjob.js <job>');

    const cronPath = require.resolve('node-cron', { paths: [path.join(api, 'services')] });
    const realCron = require(cronPath);
    let callback = null;
    require.cache[cronPath].exports = {
        validate: realCron.validate,
        schedule: (expression, fn) => {
            callback = fn;
            return { stop() {} };
        },
    };

    const pool = require(path.join(api, 'db.js'));
    const config = require(path.join(api, 'services/config.js'));
    await pool.init();
    await config.init();
    try {
        switch (job) {
        case 'cleanup':
            require(path.join(api, 'services/cleanup.js')).startCleanupScheduler();
            if (!callback) throw new Error('the cleanup scheduler did not start');
            await callback();
            break;
        case 'dream':
            require(path.join(api, 'services/dream.js')).startDreamScheduler();
            if (!callback) throw new Error('the dream scheduler did not start');
            await callback();
            break;
        default:
            throw new Error(`unknown job "${job}"`);
        }
        await settle(pool);
    } finally {
        await pool.end();
    }
}

main().catch((err) => {
    console.error('runjob:', err.stack || err.message);
    process.exit(1);
});
