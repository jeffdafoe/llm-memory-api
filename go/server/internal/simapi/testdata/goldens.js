// Writes goldens.json beside this file: what the Node server's /v1/sim
// routes and services do, so the Go port's tests compare against Node
// itself. Run from the repository root, after `npm ci` in node/api:
//
//     node go/server/internal/simapi/testdata/goldens.js
//
// The database (answering a script in order), config, logger, notes,
// invokeAgent and the trust check are stubbed; nothing leaves the process.
// narrateEvent and buildSoulUserMessage run over fixed inputs;
// distillSimConversationDay, synthesizeSimSoul and the raw-turns route
// handler run over scenarios that record one ordered stream of events:
// statements with their parameters as node-postgres prepared them, notes
// saved, model calls, trust checks, log lines and the outcome.

process.env.TZ = 'UTC';

const fs = require('fs');
const path = require('path');

const api = path.resolve(__dirname, '../../../../../node/api/src');
const { prepareValue } = require(require.resolve('pg/lib/utils', { paths: [api] }));
const pgTypes = require(require.resolve('pg-types', { paths: [api] }));
const parseTimestamptz = pgTypes.getTypeParser(1184, 'text');

function stub(rel, exports) {
    const p = require.resolve(path.join(api, rel));
    require.cache[p] = { id: p, filename: p, loaded: true, exports };
}

let sc;
let events;
const DATE_COLUMNS = new Set(['created_at']);

function answer(sql, params) {
    events.push({ kind: 'query', sql, params: (params || []).map((p) => { const v = prepareValue(p); return v === undefined ? null : v; }) });
    const step = sc.script.shift();
    if (!step) throw new Error('unscripted statement: ' + sql.trim().slice(0, 60));
    if (!sql.includes(step.match)) throw new Error(`statement does not match "${step.match}": ` + sql.trim().slice(0, 60));
    if (step.error) throw new Error(step.error);
    const rows = (step.rows || []).map((values) => {
        const row = {};
        step.columns.forEach((c, i) => {
            row[c] = DATE_COLUMNS.has(c) && typeof values[i] === 'string' ? parseTimestamptz(values[i]) : values[i];
        });
        return row;
    });
    return { rows, rowCount: rows.length };
}

stub('db.js', { async query(sql, params) { return answer(sql, params); } });
stub('services/config.js', {
    get(key) {
        if (!(key in (sc.config || {}))) throw new Error(`Config key "${key}" not found. Was config.init() called?`);
        return sc.config[key];
    },
});
stub('services/logger.js', {
    log(subsystem, action, details) { events.push({ kind: 'log', subsystem, action: typeof action === 'string' ? action : JSON.stringify(action), details: JSON.stringify(details) }); },
    logError(subsystem, action, opts) {
        const s = (v) => (v === undefined || v === null ? '' : String(v));
        events.push({ kind: 'error', subsystem, action, agent: s(opts.agent), message: s(opts.message), status: opts.statusCode || 0 });
    },
});
stub('services/documents.js', {
    async saveNote(namespace, title, content, slug, createdBy, metadata, extension, opts) {
        events.push({ kind: 'save', namespace, title, content, slug, created_by: createdBy, upsert: !!(opts && opts.upsert) });
        if (sc.saveError) throw new Error(sc.saveError);
        return { id: 1 };
    },
    async readNote() { throw new Error('not found'); },
    async listNotes() { return { notes: [] }; },
});
stub('services/virtual-agent.js', {
    async invokeAgent(agent, opts) {
        events.push({
            kind: 'invoke', agent, user: opts.userMessage, context: opts.context,
            skipRateLimit: !!opts.skipRateLimit, skipCostLimit: !!opts.skipCostLimit, skipRetry: !!opts.skipRetry,
        });
        const a = (sc.answers || []).shift();
        if (!a) throw new Error('no answer scripted');
        if (a.error) throw new Error(a.error);
        return { text: 'text' in a ? a.text : 'reply', tool_calls: [], usage: {}, cost: 0, finish_reason: a.finish || 'stop', truncated: !!a.truncated };
    },
});
stub('services/admin-permissions.js', {
    async isTrustedCreator(id) {
        if (id === null || id === undefined) return false;
        events.push({ kind: 'trust', id });
        return id === 1;
    },
    requirePerm: () => (req, res, next) => next(),
});

const distiller = require(path.join(api, 'services/sim-conversation-distiller.js'));
const simSoul = require(path.join(api, 'services/sim-soul.js'));
const router = require(path.join(api, 'routes/sim.js'));

const out = {};

// ---------------------------------------------------------------- narration

const ev = (kind, payload, extra = {}) => ({ at: '2026-11-05T10:00:00Z', kind, payload, ...extra });
const NARRATE = [
    ev('move_to', { destination: 'the [Inn]\n"Tavern"' }), ev('walked', { structure_name: 'Mill' }), ev('walked', {}), ev('walked', { destination: 5 }),
    ev('walked', { destination: ['a', null, 'b'] }), ev('walked', { destination: { x: 1 } }), ev('walked', null), ev('walked', 'str'),
    ev('chore', { type: 'well' }), ev('chore', { type: 'outhouse' }), ev('chore', { type: 'shop' }), ev('chore', { type: 'sweep  the\nfloor' }),
    ev('chore', {}), ev('chore', { type: 'constructor' }), ev('chore', { type: 'toString' }), ev('chore', { type: '__proto__' }), ev('chore', { type: 'hasOwnProperty' }),
    ev('pay', { recipient: 'Josiah', amount: 1, for: 'milk' }), ev('paid', { recipient_name: 'Moses', amount: '3', for_text: 'nails' }),
    ev('paid', { recipient: 'Gideon Marsh', amount: 1, rate_settled: 1, for: "Day's rate" }), ev('paid', { amount: 2.9, rate_settled: '0' }),
    ev('paid', { amount: -1 }), ev('paid', { amount: 'abc' }), ev('paid', { amount: 1e21 }), ev('paid', { amount: [7] }), ev('paid', { amount: true }),
    ev('object_refresh', { object_name: 'the well', refreshes: [{ attribute: 'thirst' }] }),
    ev('object_refresh', { object_name: 'oak', refreshes: [{ attribute: 'tiredness' }, { attribute: 'hunger' }, { attribute: 'joy' }] }),
    ev('object_refresh', { refreshes: 'no' }), ev('object_refresh', { refreshes: [5, 'x', {}] }), ev('object_refresh', { refreshes: [null] }),
    ev('speak', { text: 'He said "hi"\\ and\nleft' }), ev('spoke', { text: '   ' }), ev('spoke', {}), ev('spoke', { text: 0 }),
    ev('act', { verb_phrase: 'poured ale for [all]' }), ev('act', {}),
    ev('delivered', { recipient: 'John', item: 'meat', qty: 2, amount: 4 }), ev('delivered', { item: 'ale', qty: 1, amount: 0 }), ev('delivered', {}),
    ev('delivered', { item: 'bread', qty: '3.7', amount: '1' }), ev('consumed', { item: 'stew', qty: 1 }), ev('consumed', { qty: 4 }),
    ev('took_break', { reason: 'weary from\nthe day' }), ev('took_break', {}),
    ev('labored', { employer: 'Ezekiel Crane', amount: 4 }), ev('labored', { employer: '[]', amount: 0 }),
    ev('labored', { recipient: 'Josiah', amount: 2, reward_items: [{ item: 'porridge', qty: 1 }, { item: 'bread', qty: 2 }, { item: '', qty: 1 }, { item: 'x', qty: 0 }, null, 'str', [1], { item: 'ham', qty: 1.5 }] }),
    ev('labored', { amount: 0, reward_items: [{ item: 'ale', qty: 3 }] }), ev('labored', { reward_items: [] }),
    ev('solicited_work', { employer: 'Josiah Thorne', amount: 6 }), ev('offered_work', { worker: 'Constance', amount: 4 }), ev('offered_work', {}),
    ev('hired', { recipient: 'Abe', amount: 1, reward_items: [{ item: 'cheese', qty: 2 }] }),
    ev('brought_case', { parties: ['Moses James', '', '[x]', 5], complaint: 'unpaid\nrate' }), ev('brought_case', {}),
    ev('ruled', { text: 'The matter is closed.' }), ev('ruled', {}), ev('noticed_omen', { text: 'found the milk turned', subject: 'Josiah Thorne' }),
    ev('noticed_omen', { text: 'fire would not hold' }), ev('noticed_omen', { subject: 'x' }), ev('placed_ward', { text: 'nailed a horseshoe' }), ev('placed_ward', {}),
    ev('enter_huddle', {}), ev('look_around', {}), ev('done', {}), ev('gathered', {}), ev('stayed_open', {}), ev('gathered', {}),
    ev('walked', { destination: { toString: 1 } }),
];
// Each kind's first unmapped log goes to this throwaway stream.
events = [];
out.narrate = NARRATE.map((e) => {
    try {
        return { event: e, text: distiller.narrateEvent(e, 'Actor') };
    } catch (err) {
        return { event: e, error: err.message };
    }
});

out.soulMessages = [
    { characterDescription: '  Moses, a farmer.  ', currentSoul: '  Steady.  ', daySnapshot: ' day stuff ', day: '2026-11-05' },
    { characterDescription: 'x', currentSoul: '', daySnapshot: 'y', day: '' },
    { characterDescription: 'x', currentSoul: undefined, daySnapshot: 'y', day: undefined },
    { characterDescription: 'x', currentSoul: '  \n ', daySnapshot: 'y', day: '2026-01-01' },
].map((m) => ({ in: m, message: simSoul.buildSoulUserMessage(m) }));

// ---------------------------------------------------------------- scenarios

const agentStep = (rows) => ({ match: 'WHERE ac.name = $1', columns: ['id', 'name', 'dream_mode'], rows });
const SIM = (name = 'zbbs-john-ellis') => agentStep([[7, name, 'sim']]);
const SHARED = agentStep([[9, 'salem-vendor', 'sim-shared']]);
const rosterStep = { match: 'INSERT INTO sim_shared_actor' };
const lookup = (rows) => ({ match: 'ac.expertise @>', columns: ['id', 'name', 'created_by', 'provider', 'model', 'api_key'], rows });
const SOUL_AGENT = lookup([[90, 'dream-sim-soul-va', 1, 'openrouter', 'm', 'k']]);

const DAY_EVENTS = [
    ev('spoke', { text: 'Another round?' }, { at: '2026-11-05T19:00:00.000Z', speaker: 'John Ellis' }),
    ev('spoke', { text: 'Aye' }, { at: '2026-11-05T19:00:00.000Z', speaker: 'Jefferey' }),
    ev('paid', { recipient: 'John Ellis', amount: 2, for: 'ale' }, { at: '2026-11-05T08:30:00Z' }),
    ev('look_around', {}, { at: '2026-11-05T09:00:00Z' }),
    ev('walked', { destination: 'Inn' }, { at: '2026-11-04T23:59:59.999Z' }),
    ev('walked', { destination: 'Mill' }, { at: '2026-11-06T00:00:00Z' }),
    ev('walked', { destination: 'Barn' }, { at: 'not a date' }),
    ev('walked', { destination: 'Field' }, { at: 5 }),
    { kind: 'walked', payload: {} },
    null, 'x', [1],
    ev('act', { verb_phrase: 'wiped the bar' }, { at: '2026-11-05T12:00:00+05:00', speaker: '[Bad]\n"Name"' }),
    ev('act', { verb_phrase: 'swept' }, { at: '2026-11-05T12:01:00Z', speaker: 7 }),
    ev('act', { verb_phrase: 'nodded' }, { at: '2026-11-05T12:02:00Z', speaker: '' }),
];

const distills = [
    { name: 'dedicated day', input: { agent: 'zbbs-john-ellis', day: '2026-11-05', events: DAY_EVENTS }, script: [SIM()] },
    { name: 'shared day', input: { agent: 'salem-vendor', day: '2026-11-05', events: DAY_EVENTS, slugPrefix: ' constance-scott// ', actorName: '  Constance [Scott] ' }, script: [SHARED, rosterStep] },
    { name: 'empty day', input: { agent: 'zbbs-john-ellis', day: '2026-11-05', events: [ev('look_around', {})] }, script: [SIM()] },
    { name: 'shared empty day', input: { agent: 'salem-vendor', day: '2026-11-05', events: [], slugPrefix: 'a/', actorName: 'A' }, script: [SHARED] },
    { name: 'no agent', input: { agent: '', day: '2026-11-05', events: [] }, script: [] },
    { name: 'agent not a string', input: { agent: 5, day: '2026-11-05', events: [] }, script: [] },
    { name: 'bad day', input: { agent: 'x', day: '2026-11-5', events: [] }, script: [] },
    { name: 'day not a string', input: { agent: 'x', day: 20261105, events: [] }, script: [] },
    // JavaScript's $ without the m flag matches only at the end of input.
    { name: 'day with a final LF', input: { agent: 'x', day: '2026-11-05\n', events: [] }, script: [] },
    { name: 'day with a final CR', input: { agent: 'x', day: '2026-11-05\r', events: [] }, script: [] },
    { name: 'day with a final U+2028', input: { agent: 'x', day: '2026-11-05 ', events: [] }, script: [] },
    { name: 'events not an array', input: { agent: 'x', day: '2026-11-05', events: {} }, script: [] },
    { name: 'unknown agent', input: { agent: 'nobody', day: '2026-11-05', events: [] }, script: [agentStep([])] },
    { name: 'wrong mode', input: { agent: 'home', day: '2026-11-05', events: [] }, script: [agentStep([[3, 'home', 'companion']])] },
    { name: 'shared without prefix', input: { agent: 'salem-vendor', day: '2026-11-05', events: [], actorName: 'A' }, script: [SHARED] },
    { name: 'shared bad prefix', input: { agent: 'salem-vendor', day: '2026-11-05', events: [], slugPrefix: '../x/', actorName: 'A' }, script: [SHARED] },
    { name: 'shared prefix not a string', input: { agent: 'salem-vendor', day: '2026-11-05', events: [], slugPrefix: 5, actorName: 'A' }, script: [SHARED] },
    { name: 'shared without actor', input: { agent: 'salem-vendor', day: '2026-11-05', events: [], slugPrefix: 'a/', actorName: '   ' }, script: [SHARED] },
    { name: 'shared actor not a string', input: { agent: 'salem-vendor', day: '2026-11-05', events: [], slugPrefix: 'a/', actorName: 5 }, script: [SHARED] },
    { name: 'shared actor sanitized away', input: { agent: 'salem-vendor', day: '2026-11-05', events: [], slugPrefix: 'a/', actorName: '[]"' }, script: [SHARED] },
    { name: 'shared actor of 60 two-byte letters', input: { agent: 'salem-vendor', day: '2026-11-05', events: [ev('spoke', { text: 'hi' })], slugPrefix: 'a/', actorName: 'é'.repeat(60) }, script: [SHARED, rosterStep] },
    { name: 'shared actor too long', input: { agent: 'salem-vendor', day: '2026-11-05', events: [], slugPrefix: 'a/', actorName: 'é'.repeat(101) }, script: [SHARED] },
    { name: 'rolled-over day', input: { agent: 'zbbs-john-ellis', day: '2026-02-31', events: [] }, script: [SIM()] },
    { name: 'month 13', input: { agent: 'zbbs-john-ellis', day: '2026-13-01', events: [] }, script: [SIM()] },
    { name: 'leap day', input: { agent: 'zbbs-john-ellis', day: '2028-02-29', events: [ev('spoke', { text: 'leap' }, { at: '2028-02-29T23:59:00Z' })] }, script: [SIM()] },
    { name: 'save fails', saveError: 'save failed', input: { agent: 'zbbs-john-ellis', day: '2026-11-05', events: [ev('spoke', { text: 'hi' })] }, script: [SIM()] },
    { name: 'roster insert fails', input: { agent: 'salem-vendor', day: '2026-11-05', events: [ev('spoke', { text: 'hi' })], slugPrefix: 'a/', actorName: 'A' }, script: [SHARED, { match: 'INSERT INTO sim_shared_actor', error: 'roster down' }] },
    { name: 'agent query fails', input: { agent: 'x', day: '2026-11-05', events: [] }, script: [{ match: 'WHERE ac.name = $1', error: 'db down' }] },
    { name: 'narration throws', input: { agent: 'zbbs-john-ellis', day: '2026-11-05', events: [ev('object_refresh', { refreshes: [null] })] }, script: [SIM()] },
    { name: 'plain slug agent', input: { agent: 'salem-generic', day: '2026-11-05', events: [ev('spoke', { text: 'hello' })] }, script: [SIM('salem-generic')] },
];

const souls = [
    { name: 'synthesized', input: { characterDescription: 'Moses, a farmer.', currentSoul: 'Steady.', daySnapshot: 'Planted corn.', day: '2026-11-05' }, script: [SOUL_AGENT], answers: [{ text: '  A farmer of few words.  ' }] },
    { name: 'first run', input: { characterDescription: 'Moses', daySnapshot: 'Planted.' }, script: [SOUL_AGENT], answers: [{ text: 'New soul.' }] },
    { name: 'truncated', input: { characterDescription: 'M', daySnapshot: 'P' }, script: [SOUL_AGENT], answers: [{ text: 'cut', truncated: true, finish: 'length' }] },
    { name: 'empty reply', input: { characterDescription: 'M', daySnapshot: 'P' }, script: [SOUL_AGENT], answers: [{ text: '  ' }] },
    { name: 'null reply', input: { characterDescription: 'M', daySnapshot: 'P' }, script: [SOUL_AGENT], answers: [{ text: null }] },
    { name: 'array reply', input: { characterDescription: 'M', daySnapshot: 'P' }, script: [SOUL_AGENT], answers: [{ text: [1] }] },
    { name: 'reasoning', input: { characterDescription: 'M', daySnapshot: 'P' }, script: [SOUL_AGENT], answers: [{ text: 'Thinking Process: first...' }] },
    { name: 'invoke fails', input: { characterDescription: 'M', daySnapshot: 'P' }, script: [SOUL_AGENT], answers: [{ error: 'Agent dream-sim-soul-va is rate-limited' }] },
    { name: 'no soul agent', input: { characterDescription: 'M', daySnapshot: 'P' }, script: [lookup([])] },
    { name: 'untrusted soul agent', input: { characterDescription: 'M', daySnapshot: 'P' }, script: [lookup([[90, 'x', 5, 'p', 'm', 'k']])] },
    { name: 'missing description', input: { daySnapshot: 'P' }, script: [] },
    { name: 'blank description', input: { characterDescription: '  ', daySnapshot: 'P' }, script: [] },
    { name: 'description not a string', input: { characterDescription: 5, daySnapshot: 'P' }, script: [] },
    { name: 'description too long', input: { characterDescription: 'é'.repeat(100001), daySnapshot: 'P' }, script: [] },
    { name: 'missing snapshot', input: { characterDescription: 'M' }, script: [] },
    { name: 'soul not a string', input: { characterDescription: 'M', daySnapshot: 'P', currentSoul: 5 }, script: [] },
    { name: 'soul null', input: { characterDescription: 'M', daySnapshot: 'P', currentSoul: null }, script: [SOUL_AGENT], answers: [{ text: 'ok' }] },
    { name: 'soul too long', input: { characterDescription: 'M', daySnapshot: 'P', currentSoul: 'x'.repeat(100001) }, script: [] },
    { name: 'bad day', input: { characterDescription: 'M', daySnapshot: 'P', day: '2026-11-05\n## Inject' }, script: [] },
    { name: 'day not a string', input: { characterDescription: 'M', daySnapshot: 'P', day: 5 }, script: [] },
    { name: 'day with a final LF', input: { characterDescription: 'M', daySnapshot: 'P', day: '2026-11-05\n' }, script: [] },
    { name: 'day with a final U+2029', input: { characterDescription: 'M', daySnapshot: 'P', day: '2026-11-05 ' }, script: [] },
    { name: 'empty day', input: { characterDescription: 'M', daySnapshot: 'P', day: '' }, script: [SOUL_AGENT], answers: [{ text: 'ok' }] },
];

// The raw-turns handler, called as Express would: the route's own layer
// (requirePerm is stubbed through), with a recording res.
const rawTurnsLayer = router.stack.find((l) => l.route && l.route.path === '/sim/raw-turns');
const rawTurnsHandler = rawTurnsLayer.route.stack[rawTurnsLayer.route.stack.length - 1].handle;
const TURN_COLS = ['id', 'agent', 'scene_id', 'context', 'context_id', 'provider', 'model', 'sim_actor_id', 'sim_actor_name',
    'system_prompt', 'user_message', 'response', 'status', 'status_code', 'error_message',
    'input_tokens', 'output_tokens', 'cache_read_tokens', 'cache_write_tokens', 'cost', 'duration_ms', 'created_at'];
const turn = (id, extra = {}) => {
    const base = {
        id: String(id), agent: 'zbbs-john-ellis', scene_id: '6d2a0c1e-0000-4000-8000-0000000000a1', context: 'chat', context_id: null,
        provider: 'openrouter', model: 'm', sim_actor_id: 'a1', sim_actor_name: 'John', system_prompt: 'sys', user_message: 'user',
        response: 'resp', status: 'success', status_code: null, error_message: null, input_tokens: 10, output_tokens: 5,
        cache_read_tokens: 0, cache_write_tokens: 0, cost: '0.000035', duration_ms: 120, created_at: '2026-11-05 10:00:00.123456+00',
        ...extra,
    };
    return TURN_COLS.map((c) => base[c]);
};
const turnsStep = (n) => ({ match: 'FROM virtual_agent_calls c', columns: TURN_COLS, rows: Array.from({ length: n }, (_, i) => turn(100 - i)) });
const raws = [
    { name: 'unbounded default', body: {}, script: [turnsStep(6)] },
    { name: 'unbounded under the limit', body: { limit: 3 }, script: [turnsStep(2)] },
    { name: 'exactly the limit', body: { limit: 3 }, script: [turnsStep(3)] },
    { name: 'scene', body: { scene_id: '6D2A0C1E-0000-4000-8000-0000000000A1' }, script: [turnsStep(1)] },
    { name: 'bad scene', body: { scene_id: 'nope' }, script: [] },
    { name: 'scene number', body: { scene_id: 12345 }, script: [] },
    { name: 'scene with a final LF', body: { scene_id: '6d2a0c1e-0000-4000-8000-0000000000a1\n' }, script: [] },
    { name: 'agent', body: { agent: 'zbbs-john-ellis', limit: '7' }, script: [turnsStep(8)] },
    { name: 'agent not a string', body: { agent: 5 }, script: [] },
    { name: 'agent too long', body: { agent: 'a'.repeat(201) }, script: [] },
    { name: 'conversation', body: { conversation: 'hud-abc', limit: 1000 }, script: [turnsStep(0)] },
    { name: 'conversation not a string', body: { conversation: ['x'] }, script: [] },
    { name: 'conversation too long', body: { conversation: 'c'.repeat(201) }, script: [] },
    { name: 'sim actor trimmed', body: { sim_actor: '  alice  ' }, script: [turnsStep(1)] },
    { name: 'sim actor blank', body: { sim_actor: '   ' }, script: [turnsStep(0)] },
    { name: 'sim actor not a string', body: { sim_actor: 1 }, script: [] },
    { name: 'sim actor too long', body: { sim_actor: 's'.repeat(101) }, script: [] },
    { name: 'since and until', body: { since: '2026-11-05T10:00:00.5Z', until: '2026-11-06', status: 'error' }, script: [turnsStep(0)] },
    { name: 'since number', body: { since: 1793990000000 }, script: [turnsStep(0)] },
    { name: 'bad since', body: { since: 'yesterday' }, script: [] },
    { name: 'bad until', body: { until: {} }, script: [] },
    { name: 'bad status', body: { status: 'ok' }, script: [] },
    { name: 'empty filters', body: { scene_id: '', agent: null, conversation: '', sim_actor: null, since: '', until: null, status: '' }, script: [turnsStep(0)] },
    { name: 'limit odd', body: { limit: '4.5' }, script: [turnsStep(0)] },
    { name: 'limit zero', body: { limit: 0 }, script: [turnsStep(2)] },
    { name: 'limit negative', body: { limit: -3 }, script: [turnsStep(0)] },
    { name: 'limit too high', body: { limit: 99 }, script: [turnsStep(0)] },
    { name: 'limit blank', body: { limit: '  ' }, script: [turnsStep(0)] },
    { name: 'limit true', body: { limit: true }, script: [turnsStep(0)] },
    { name: 'all filters', body: { scene_id: '6d2a0c1e-0000-4000-8000-0000000000a1', agent: 'a', conversation: 'c', sim_actor: 's', since: '2026-01-01', until: '2026-12-01', status: 'success', limit: 2 }, script: [turnsStep(3)] },
    { name: 'query fails', body: {}, script: [{ match: 'FROM virtual_agent_calls c', error: 'db down' }] },
    { name: 'no body', body: undefined, script: [turnsStep(0)] },
];

function recordingRes() {
    const res = { statusCode: 200 };
    res.status = (c) => { res.statusCode = c; return res; };
    res.json = (b) => { res.body = JSON.stringify(b); return res; };
    return res;
}

async function run(list, fn) {
    const results = [];
    for (const s of list) {
        sc = { ...s, script: s.script.map((x) => ({ ...x })), answers: [...(s.answers || [])] };
        events = [];
        let outcome;
        try {
            outcome = { value: JSON.stringify(await fn(s)) };
        } catch (err) {
            outcome = { error: err.message, status: err.statusCode || 0 };
        }
        results.push({ ...s, unused: sc.script.length, events, outcome });
    }
    return results;
}

(async () => {
    out.distills = await run(distills, (s) => distiller.distillSimConversationDay(s.input.agent, s.input.day, s.input.events, { slugPrefix: s.input.slugPrefix, actorName: s.input.actorName }));
    out.souls = await run(souls, (s) => simSoul.synthesizeSimSoul(s.input));
    out.raws = await run(raws, async (s) => {
        const res = recordingRes();
        await rawTurnsHandler({ body: s.body, authenticatedAgent: 'home' }, res);
        return { status: res.statusCode, body: res.body };
    });
    for (const list of [out.distills, out.souls, out.raws]) {
        for (const r of list) {
            if (r.unused) console.error(`${r.name}: ${r.unused} statements unused`);
            if (r.outcome.error && /unscripted|does not match|no answer/.test(r.outcome.error)) console.error(`${r.name}: ${r.outcome.error}`);
        }
    }
    fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify(out, null, 1) + '\n');
})();
