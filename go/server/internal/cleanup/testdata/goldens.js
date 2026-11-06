// Writes goldens.json beside this file: what Node's cleanup job does for a
// set of scenarios, so the Go port is compared with Node itself. Each
// scenario sets the config, scripts the database's answers in order, runs
// the real cron callback (or startCleanupScheduler alone), and records every
// statement with its parameters as node-postgres sent them, the log lines,
// and the error_log calls. Run from the repository root, after `npm ci` in
// node/api:
//
//     node go/server/internal/cleanup/testdata/goldens.js
//
// The database is a stub; nothing leaves the process.

const fs = require('fs');
const path = require('path');

const api = path.resolve(__dirname, '../../../../../node/api/src');
const { prepareValue } = require(path.join(api, '../node_modules/pg/lib/utils'));

// One scenario at a time: the stub answers from `script` in order and
// refuses a statement that does not contain the scripted `match`.
let script = [];
let queries = [];
const timeColumns = new Set(['created_at', 'updated_at', 'last_accessed']);
const pool = {
    query: async (sql, params) => {
        queries.push({ sql, params: (params || []).map((p) => prepareValue(p)) });
        const step = script.shift();
        if (!step) throw new Error('unscripted statement: ' + sql.trim().slice(0, 60));
        if (!sql.includes(step.match)) throw new Error(`statement does not match "${step.match}": ` + sql.trim().slice(0, 60));
        if (step.error) throw new Error(step.error);
        const rows = (step.rows || []).map((values) => {
            const row = {};
            step.columns.forEach((c, i) => {
                row[c] = timeColumns.has(c) && values[i] !== null ? new Date(values[i]) : values[i];
            });
            return row;
        });
        return { rows, rowCount: step.rowCount ?? rows.length };
    },
};
const dbPath = require.resolve(path.join(api, 'db.js'));
require.cache[dbPath] = { id: dbPath, filename: dbPath, loaded: true, exports: pool };

let logs = [];
let errors = [];
const loggerPath = require.resolve(path.join(api, 'services/logger.js'));
require.cache[loggerPath] = {
    id: loggerPath, filename: loggerPath, loaded: true,
    exports: {
        log: (subsystem, action, details) => logs.push([subsystem, action, JSON.stringify(details)]),
        logError: (subsystem, action, opts) => errors.push([subsystem, action, opts.message]),
    },
};

// node-cron: the real validate and schedule, except that schedule hands the
// callback back and stops the timer, so the scenario runs the pass itself.
const cronPath = require.resolve('node-cron', { paths: [path.join(api, 'services')] });
const realCron = require(cronPath);
let scheduled = null;
require.cache[cronPath].exports = {
    validate: realCron.validate,
    schedule: (expression, fn, options) => {
        const task = realCron.schedule(expression, fn, options);
        task.stop();
        scheduled = { expression, fn };
        return task;
    },
};

const config = require(path.join(api, 'services/config.js'));
let cfg = {};
config.get = (key) => {
    if (!(key in cfg)) throw new Error(`Config key "${key}" not found. Was config.init() called?`);
    return cfg[key];
};

const { startCleanupScheduler } = require(path.join(api, 'services/cleanup.js'));

const BASE = {
    cleanup_cron_schedule: '0 5 * * *',
    cleanup_enabled: 'true',
    cleanup_decay_threshold: '0.05',
    search_decay_halflife_task: '60',
    search_decay_halflife_learning: '90',
    search_decay_halflife_note: '180',
    search_decay_halflife_conversation: '30',
    search_decay_halflife_dream: '30',
    search_decay_halflife_episodic: '90',
    search_decay_halflife_reflective: '180',
    va_call_log_retention_days: '5',
    conversation_retention_days: '30',
    // Off unless a scenario turns it on, so the older scenarios script no
    // sim chat statements.
    sim_chat_retention_days: '0',
};
const ZERO_HALF_LIVES = {
    search_decay_halflife_task: '0', search_decay_halflife_learning: '0', search_decay_halflife_note: '0',
    search_decay_halflife_conversation: '0', search_decay_halflife_dream: '0',
    search_decay_halflife_episodic: '0', search_decay_halflife_reflective: '0',
};
// Purge and retention off, so a scenario about decay scripts only decay.
const DECAY_ONLY = { va_call_log_retention_days: '0', conversation_retention_days: '' };

const decayColumns = ['id', 'namespace', 'slug', 'kind', 'cognitive_type', 'created_at', 'updated_at', 'last_accessed'];
const select = (rows) => ({ match: 'FROM documents d', columns: decayColumns, rows });
const retire = (length) => ({ match: 'UPDATE documents SET deleted_at = NOW() WHERE id = $1', columns: ['content_length'], rows: length === undefined ? [] : [[length]] });
const usage = { match: 'INSERT INTO namespace_usage', rowCount: 1 };
const chunks = (n = 1) => ({ match: 'DELETE FROM memory_chunks', rowCount: n });
const purge = (n) => ({ match: 'DELETE FROM virtual_agent_calls', columns: ['id'], rows: Array.from({ length: n }, (_, i) => [i + 1]) });
const stage1 = (rows) => ({ match: "SET deleted_at = NOW()\n         WHERE kind = 'conversation'", columns: ['namespace', 'slug', 'content_length'], rows });
const stage2 = (rows) => ({ match: "SET content = ''", columns: ['namespace', 'slug'], rows });
const engine = (rows) => ({ match: 'SELECT id FROM actors WHERE name = $1', columns: ['id'], rows });
const simPurge = (n) => ({ match: 'DELETE FROM chat_message_texts WHERE id IN', rowCount: n });
// Only the sim chat task on.
const SIM_ONLY = { cleanup_enabled: 'false', va_call_log_retention_days: '0', conversation_retention_days: '', sim_chat_retention_days: '14' };

const scenarios = [
    {
        name: 'full pass',
        config: { sim_chat_retention_days: '14' },
        script: [
            select([
                [11, 'agent-a', 'notes/old', 'note', null, '2025-01-01T00:00:00.123456Z', '2025-01-02T03:04:05.000Z', null],
                [12, 'shared', 'learnings/x', 'learning', ' Episodic ', '2024-06-01T10:00:00.000Z', null, '2024-07-01T00:00:00.999999Z'],
            ]),
            retire(120), usage, chunks(2),
            retire(), chunks(0),
            purge(3),
            stage1([['agent-a', 'conversations/s1', 500], ['agent-b', 'conversations/s2', null], ['agent-a', 'conversations/s3', 70]]),
            chunks(), chunks(), chunks(), usage, usage,
            stage2([['agent-a', 'conversations/old']]), chunks(0),
            engine([[46]]), simPurge(2),
        ],
    },
    { name: 'everything off', config: { cleanup_enabled: 'false', va_call_log_retention_days: '0', conversation_retention_days: '' }, script: [] },
    { name: 'cleanup_enabled TRUE is off', config: { cleanup_enabled: 'TRUE', ...DECAY_ONLY }, script: [] },
    { name: 'nothing decays, garbage retention values', config: { ...ZERO_HALF_LIVES, va_call_log_retention_days: 'abc', conversation_retention_days: '-1' }, script: [] },
    { name: 'missing half-life key', config: { search_decay_halflife_note: undefined, ...DECAY_ONLY }, script: [] },
    { name: 'missing purge and retention keys', config: { cleanup_enabled: 'false', va_call_log_retention_days: undefined, conversation_retention_days: undefined }, script: [] },
    { name: 'missing threshold key', config: { cleanup_decay_threshold: undefined, ...DECAY_ONLY }, script: [] },
    { name: 'missing enabled key', config: { cleanup_enabled: undefined, ...DECAY_ONLY }, script: [] },
    { name: 'nothing found', config: {}, script: [select([]), purge(0), stage1([]), stage2([])] },
    { name: 'decay select fails', config: DECAY_ONLY, script: [{ match: 'FROM documents d', error: 'canceling statement due to statement timeout' }] },
    {
        name: 'decay row errors',
        config: DECAY_ONLY,
        script: [
            select([
                [21, 'agent-a', 'notes/a', 'note', null, '2025-01-01T00:00:00.000Z', null, null],
                [22, 'agent-a', 'notes/b', 'note', null, '2025-01-01T00:00:00.000Z', null, null],
                [23, 'agent-a', 'notes/c', 'note', null, '2025-01-01T00:00:00.000Z', null, null],
            ]),
            { match: 'UPDATE documents SET deleted_at = NOW() WHERE id = $1', error: 'deadlock detected' },
            retire(5), usage, { match: 'DELETE FROM memory_chunks', error: 'could not serialize access' },
            retire(0), usage, chunks(),
        ],
    },
    { name: 'purge fails', config: { cleanup_enabled: 'false', conversation_retention_days: '' }, script: [{ match: 'DELETE FROM virtual_agent_calls', error: 'interval out of range' }] },
    { name: 'retention stage 1 fails', config: { cleanup_enabled: 'false', va_call_log_retention_days: '0' }, script: [{ match: "SET deleted_at = NOW()\n         WHERE kind", error: 'relation "documents" does not exist' }] },
    { name: 'sim chat one batch', config: SIM_ONLY, script: [engine([[46]]), simPurge(7)] },
    { name: 'sim chat several batches', config: SIM_ONLY, script: [engine([[46]]), simPurge(5000), simPurge(5000), simPurge(0)] },
    { name: 'sim chat nothing old', config: SIM_ONLY, script: [engine([[46]]), simPurge(0)] },
    { name: 'sim chat no engine actor', config: SIM_ONLY, script: [engine([])] },
    { name: 'sim chat engine lookup fails', config: SIM_ONLY, script: [{ match: 'SELECT id FROM actors WHERE name = $1', error: 'connection terminated' }] },
    { name: 'sim chat purge fails', config: SIM_ONLY, script: [engine([[46]]), simPurge(5000), { match: 'DELETE FROM chat_message_texts WHERE id IN', error: 'interval out of range' }] },
    { name: 'sim chat missing key', config: { ...SIM_ONLY, sim_chat_retention_days: undefined }, script: [] },
    { name: 'retention only tombstones', config: { cleanup_enabled: 'false', va_call_log_retention_days: '0', conversation_retention_days: '0' }, script: [stage1([]), stage2([['a', 'conversations/x'], ['b', 'conversations/y']]), chunks(), chunks()] },
];

// cleanup_decay_threshold is parseFloat(x) || 0.05.
for (const threshold of ['abc', '0', '-0', '-0.5', ' 0.1x', 'Infinity', '-Infinity', '1e-3', '.5', '5.', '0x10', '']) {
    scenarios.push({ name: `threshold ${JSON.stringify(threshold)}`, config: { cleanup_decay_threshold: threshold, ...DECAY_ONLY }, script: [select([])] });
}

// The half-life readers (the Node unit tests' cases and more).
const halfLives = [
    { search_decay_halflife_note: '45' },
    { search_decay_halflife_episodic: '0', search_decay_halflife_reflective: '180' },
    { search_decay_halflife_episodic: '', search_decay_halflife_reflective: '   ' },
    { search_decay_halflife_note: '-45', search_decay_halflife_reflective: '-1' },
    { search_decay_halflife_note: '45', search_decay_halflife_dream: '30', search_decay_halflife_episodic: '90' },
    { search_decay_halflife_note: '', search_decay_halflife_dream: 'soon' },
    { search_decay_halflife_task: '0x10', search_decay_halflife_learning: '1e2', search_decay_halflife_note: '.5' },
    { search_decay_halflife_conversation: ' 7 ', search_decay_halflife_dream: '-0', search_decay_halflife_episodic: 'Infinity' },
    { search_decay_halflife_reflective: '-Infinity', search_decay_halflife_note: '1e400' },
    { search_decay_halflife_episodic: '12345678901234567890', search_decay_halflife_note: '0.1' },
];
// A scenario whose rules all come out off never runs the SELECT, and its
// scripted answer is left over (`unused`).
for (const values of halfLives) {
    scenarios.push({
        name: `half-lives ${JSON.stringify(values)}`,
        config: { ...ZERO_HALF_LIVES, ...values, ...DECAY_ONLY },
        script: [select([])],
    });
}

// Retention and purge numbers: parseInt for the purge, Number() and
// Number.isInteger for retention, both bound as JavaScript number text.
const numbers = [
    ['1e3', '1e21'], ['99999999999999999999999', '-0'], [' 7days', ' 7 '], ['0x1A', '0x10'], ['-5', '30.5'],
    ['  12', 'Infinity'], ['3.9', '1e2'], ['', '007'],
];
for (const [purgeDays, retentionDays] of numbers) {
    const purgeOn = Number.isFinite(parseInt(purgeDays)) && parseInt(purgeDays) > 0;
    const n = Number(retentionDays);
    const retentionOn = String(retentionDays).trim() !== '' && Number.isInteger(n) && n >= 0;
    const s = [];
    if (purgeOn) s.push(purge(1));
    if (retentionOn) s.push(stage1([]), stage2([]));
    scenarios.push({
        name: `numbers purge ${JSON.stringify(purgeDays)} retention ${JSON.stringify(retentionDays)}`,
        config: { cleanup_enabled: 'false', va_call_log_retention_days: purgeDays, conversation_retention_days: retentionDays },
        script: s,
    });
}

// sim_chat_retention_days is parseInt(x) || 0, on above 0.
for (const days of ['abc', '-3', ' 7days', '1e3', '0x10', '3.9', '', '99999999999999999999999']) {
    const on = Number.isFinite(parseInt(days)) && parseInt(days) > 0;
    scenarios.push({
        name: `sim chat days ${JSON.stringify(days)}`,
        config: { ...SIM_ONLY, sim_chat_retention_days: days },
        script: on ? [engine([[46]]), simPurge(1)] : [],
    });
}

// startCleanupScheduler alone.
const starts = ['0 5 * * *', '', ' ', 'bogus', '0 0 30W 2 *', '@daily', '61 * * * *', undefined];

async function main() {
    const out = { pass: [], start: [] };
    for (const sc of scenarios) {
        cfg = { ...BASE, ...sc.config };
        for (const [k, v] of Object.entries(cfg)) if (v === undefined) delete cfg[k];
        script = sc.script.map((s) => ({ ...s }));
        queries = [];
        logs = [];
        errors = [];
        scheduled = null;
        startCleanupScheduler();
        if (!scheduled) throw new Error(`${sc.name}: scheduler did not start`);
        logs = [];
        await scheduled.fn();
        // updateUsage is fire-and-forget; let its promise settle.
        await new Promise((resolve) => setImmediate(resolve));
        const unused = script.length;
        const config = {};
        for (const [k, v] of Object.entries(cfg)) config[k] = v;
        out.pass.push({ name: sc.name, config, script: sc.script, unused, queries, logs, errors });
    }
    for (const schedule of starts) {
        cfg = { ...BASE, cleanup_cron_schedule: schedule };
        if (schedule === undefined) delete cfg.cleanup_cron_schedule;
        logs = [];
        scheduled = null;
        let threw = '';
        try {
            startCleanupScheduler();
        } catch (err) {
            threw = err.message;
        }
        out.start.push({ schedule: schedule === undefined ? null : schedule, started: scheduled !== null, threw, logs });
    }
    fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify(out, null, 1) + '\n');
    console.log(`wrote ${out.pass.length} pass and ${out.start.length} start scenarios`);
}

main().catch((err) => {
    console.error(err);
    process.exit(1);
});
