// Writes goldens_discussion.json beside this file: what the Node server's
// virtual-agent service does when a discussion's virtual participants are
// triggered (handleVirtualAgent and the functions only it uses), so the Go
// port's tests compare against Node itself. Run from the repository root,
// after `npm ci` in node/api:
//
//     node go/server/internal/virtualagent/testdata/goldens_discussion.js
//
// Same approach as goldens.js and goldens_chat.js: the database, config,
// logger, events, actors, notes, memory search, chat, the discussion service
// and the provider call are stubbed and recorded; nothing leaves the
// process. chatSend, evaluateVote and discussionLeave are recorded as query
// kinds (chat-send, evaluate-vote, discussion-leave), so their place among
// the writes is compared.

process.env.TZ = 'Asia/Kolkata';

const fs = require('fs');
const path = require('path');
const Module = require('module');

const api = path.resolve(__dirname, '../../../../../node/api/src');
const { prepareValue } = require(require.resolve('pg/lib/utils', { paths: [api] }));

function stub(rel, exports) {
    const p = require.resolve(path.join(api, rel));
    require.cache[p] = { id: p, filename: p, loaded: true, exports };
}

let rec;
let scenario;
// The module the scenario runs on; the provider stub calls back into it for
// a trigger that arrives during a generation.
let current;

const KINDS = [
    ['load-agent', /SELECT ac\.id AS actor_id, ac\.name AS agent/],
    ['rate-overrides', /SELECT ac\.configuration FROM agent_configuration/],
    ['cost-sums', /SUM\(CASE WHEN created_at/],
    ['usage-insert', /INSERT INTO virtual_agent_usage/],
    ['call-insert', /INSERT INTO virtual_agent_calls/],
    ['active-on', /UPDATE actors SET active_since = NOW\(\)/],
    ['active-off', /UPDATE actors SET active_since = NULL/],
    ['status', /UPDATE actors SET status/],
    ['dream-mode', /SELECT agc\.dream_mode/],
    ['discussion-load', /SELECT \* FROM discussions WHERE id = \$1/],
    ['participants-load', /FROM discussion_participants dp/],
    ['discussion-history', /SELECT DISTINCT ON \(cmt\.id\)/],
    ['vote-question', /SELECT question FROM discussion_votes/],
    ['ballot-check', /SELECT 1 FROM discussion_ballots/],
    ['ballot-insert', /INSERT INTO discussion_ballots/],
];

function kindOf(sql) {
    for (const [kind, re] of KINDS) {
        if (re.test(sql)) return kind;
    }
    throw new Error('unclassified query: ' + sql);
}

// record logs one query (or one stubbed service call), fails it as the
// scenario says (failNth: the nth call of a kind; null passes; fail: every
// call), and answers from the scenario's rows: rowsBy[kind][first param]
// when given, else rows[kind] (a queue when it is an array of arrays; the
// last answer repeats).
function record(kind, params) {
    const prepared = (params || []).map(p => { const v = prepareValue(p); return v === undefined ? null : v; });
    const q = { kind, params: prepared };
    if (inTimer) q.timer = true;
    rec.queries.push(q);
    const n = (rec.nth[kind] = (rec.nth[kind] || 0) + 1);
    const nth = (scenario.failNth || {})[kind] || [];
    if (n <= nth.length && nth[n - 1] !== null) throw new Error(nth[n - 1]);
    const fail = (scenario.fail || {})[kind];
    if (fail) throw new Error(fail);
    const by = (scenario.rowsBy || {})[kind];
    if (by) {
        const key = String(prepared[0]);
        return { rows: JSON.parse(JSON.stringify(by[key] || [])) };
    }
    const rows = (scenario.rows || {})[kind];
    let answer = rows || [];
    if (Array.isArray(rows) && rows.length > 0 && Array.isArray(rows[0])) {
        const c = (rec.counts[kind] = (rec.counts[kind] || 0) + 1);
        answer = rows[Math.min(c, rows.length) - 1];
    }
    answer = JSON.parse(JSON.stringify(answer));
    // Scenarios list a discussion's history oldest first; the query
    // (ORDER BY id DESC) returns it newest first.
    if (kind === 'discussion-history') answer.reverse();
    return { rows: answer };
}

const sqlByKind = {};
stub('db.js', {
    async query(sql, params) {
        const kind = kindOf(sql);
        sqlByKind[kind] = sql.replace(/\s+/g, ' ').trim();
        return record(kind, params);
    },
});

let configValues = {};
stub('services/config.js', {
    get(key) {
        if (!(key in configValues)) {
            throw new Error(`Config key "${key}" not found. Was config.init() called?`);
        }
        return configValues[key];
    },
});
stub('services/logger.js', {
    log(subsystem, action, details) { rec.logs.push({ subsystem, action, details: JSON.stringify(details) }); },
    // The stack trace (detail) is not compared.
    logError(subsystem, action, opts) {
        const { agent, context, contextId, message, statusCode } = opts;
        rec.errors.push({ subsystem, action, details: JSON.stringify({ agent, context, contextId, message, statusCode }) });
    },
});
stub('services/events.js', { broadcast(event, data) { rec.events.push({ event, data: JSON.stringify(data) }); } });
const ACTOR_IDS = { system: 1, prudence: 7, 'ezekiel-crane': 8, wendy: 13, home: 14 };
stub('services/actors.js', {
    async requireByName(name) {
        if (!(name in ACTOR_IDS)) {
            throw Object.assign(new Error(`Agent "${name}" is not registered`), { statusCode: 404 });
        }
        return { id: ACTOR_IDS[name], name };
    },
});
stub('services/documents.js', {
    async saveNote(namespace, title, content, slug, createdBy) {
        rec.notes.push({ namespace, title, content, slug: slug.replace(/-[0-9a-z]{0,4}$/, '-XXXX'), created_by: createdBy });
        return { id: 1 };
    },
    async readNote(namespace, slug) {
        rec.reads.push({ namespace, slug });
        const notes = scenario.notesIn || {};
        if (!(slug in notes)) {
            throw Object.assign(new Error(`Note not found: ${slug}`), { statusCode: 404 });
        }
        return { content: notes[slug] };
    },
});
stub('services/memory.js', {
    async searchMemory(query, namespace, limit) {
        rec.searches.push({ query, namespace, limit });
        if (scenario.searchError) throw new Error(scenario.searchError);
        return JSON.parse(JSON.stringify(scenario.search || { results: [] }));
    },
});
// chatSend as chat.js read its arguments: from, to_agents, the discussion id
// as its query parameter (String), the message, isError.
stub('services/chat.js', {
    async chatSend(from, toAgents, discussionId, message, opts) {
        const o = opts || {};
        record('chat-send', [from, JSON.stringify(toAgents), discussionId == null ? null : String(discussionId), message, o.isError === true]);
        return {};
    },
});
stub('services/discussion.js', {
    async evaluateVote(voteId) {
        record('evaluate-vote', [JSON.stringify(voteId)]);
    },
    async discussionLeave(discussionId, agent) {
        record('discussion-leave', [JSON.stringify(discussionId), agent]);
        return {};
    },
});
stub('services/system-handler.js', { register() {} });

const realProvider = require(path.join(api, 'services/provider.js'));
stub('services/provider.js', {
    ...realProvider,
    decryptApiKey: (k) => 'dec:' + k,
    createProvider(provider, model, apiKey, conf) {
        if (provider === 'unknown-provider') throw new Error('Unsupported provider: ' + provider);
        rec.created.push({ provider, model, api_key: apiKey, conf: JSON.stringify(conf) });
        return async (system, user, opts) => {
            rec.calls.push({
                system: typeof system === 'string' ? { text: system } : { static: system.static, dynamic: system.dynamic },
                user,
                opts: JSON.stringify(opts),
            });
            const n = rec.callIndex++;
            const answers = scenario.answers || [];
            const a = answers[Math.min(n, answers.length - 1)];
            // Triggers that arrive while this generation runs (not when the
            // last answer repeats).
            for (const p of (n < answers.length ? a.during : null) || []) {
                rec.handled.push(await handle(p));
            }
            if (a.error) {
                const err = new Error(a.error);
                if (a.status !== undefined) err.status = a.status;
                throw err;
            }
            return JSON.parse(JSON.stringify(a.result));
        };
    },
});

let clock = 0;
const RealDate = Date;
global.Date = class extends RealDate {
    constructor(...args) {
        if (args.length === 0) super(clock);
        else super(...args);
    }
    static now() { return clock; }
};
// setTimeout runs at once and records its delay (the response delay and
// stagger, the activity spinner's off-switch).
let inTimer = false;
global.setTimeout = (fn, ms) => {
    rec.timers.push(ms);
    Promise.resolve().then(() => {
        inTimer = true;
        try { fn(); } finally { inTimer = false; }
    });
    return { unref() {} };
};

const file = path.join(api, 'services/virtual-agent.js');
const src = fs.readFileSync(file, 'utf8') + '\nmodule.exports.__t = { renderDiscussionContext, buildSystemPrompt, buildUserMessage, parseVoteResponse };\n';
function freshModule() {
    const m = new Module(file, module);
    m.filename = file;
    m.paths = Module._nodeModulePaths(path.dirname(file));
    m._compile(src, file);
    return m.exports;
}
const t = freshModule().__t;

async function handle(payload) {
    try {
        await current.handleVirtualAgent(payload);
        return { ok: null };
    } catch (err) {
        return { error: err.message };
    }
}

const BASE_CONFIG = {
    virtual_agent_rate_limit: '10',
    virtual_agent_rate_window_seconds: '60',
    virtual_agent_cooldown_seconds: '300',
    virtual_agent_default_daily_budget: '1.00',
    virtual_agent_default_monthly_budget: '10.00',
    virtual_agent_retry_backoff: '300,600,3600',
    virtual_agent_retry_backoff_client_error: '5,15,60',
    virtual_agent_learning_enabled: 'false',
    virtual_agent_learning_min_tokens: '20',
    virtual_agent_error_ping_interval: '15',
    virtual_agent_response_delay_seconds: '0',
    virtual_agent_response_stagger_seconds: '0',
    global_bootstrap: 'Be kind.',
};

const CLOCK = 1793990000000;
const iso = (msAgo) => new RealDate(CLOCK - msAgo).toISOString();

function agentRow(name, over) {
    return Object.assign({
        actor_id: ACTOR_IDS[name], agent: name, virtual: true, provider: 'anthropic', model: 'claude-haiku-4-5',
        api_key: 'enc', configuration: '{"_configVersion":1,"max_tokens":256}',
        startup_instructions: 'You are ' + name + '.', personality: null, expertise: [],
        cost_budget_daily: null, cost_budget_monthly: null, cache_prompts: false,
        learning_enabled: true, max_tokens: null, temperature: null,
    }, over || {});
}
const humanRow = (name) => agentRow(name, { virtual: false, provider: null, model: null, api_key: null, configuration: null });

// The discussions row, in its column order (SELECT *).
function discussionRow(over) {
    return Object.assign({
        id: 101, topic: 'Plan the harvest fair', status: 'active', channel: null, created_at: iso(3600000),
        concluded_at: null, mode: 'realtime', context: null, timeout_at: iso(-3600000), outcome: null, created_by_actor_id: 13,
    }, over || {});
}
function part(agent, status, role) {
    return { agent, status: status || 'joined', role: role || 'required' };
}
function msg(id, from, message, msAgo) {
    return { id, from_agent: from, message, sent_at: iso(msAgo === undefined ? 1000 * (100 - id) : msAgo) };
}

const ANSWER = { result: { text: 'A fine idea.', tool_calls: [], usage: { input_tokens: 120, output_tokens: 30 }, finish_reason: 'end_turn', truncated: false } };
function answer(text) {
    return { result: Object.assign({}, ANSWER.result, { text }) };
}

const AGENTS = { prudence: [agentRow('prudence')], 'ezekiel-crane': [agentRow('ezekiel-crane')], wendy: [humanRow('wendy')], home: [] };
const PARTS = [part('wendy'), part('prudence'), part('ezekiel-crane')];

// rows for a discussion with wendy (human) and two virtual agents.
function baseRows(over) {
    return Object.assign({
        'discussion-load': [discussionRow()],
        'participants-load': PARTS,
        'cost-sums': [{ cost_today: '0', cost_monthly: '0' }],
        'usage-insert': [{ id: 41 }],
        'dream-mode': [],
        'ballot-check': [],
    }, over || {});
}
function agentsBy(over) {
    return { 'load-agent': Object.assign({}, AGENTS, over || {}) };
}

const message = (id) => ({ type: 'virtual-agent', discussionId: id === undefined ? 101 : id, triggerType: 'message' });
const active = { type: 'virtual-agent', discussionId: 101, triggerType: 'discussion-active' };
const vote = (voteId) => ({ type: 'virtual-agent', discussionId: 101, triggerType: 'vote-proposed', voteId: voteId === undefined ? 55 : voteId });

const HISTORY = [msg(90, 'wendy', 'Good evening, friends. What shall we bring to the fair?')];
const ONLY_PRUDENCE = [{ ...msg(90, 'wendy', 'Good evening.') }, msg(91, 'ezekiel-crane', 'Evening.'), msg(92, 'wendy', 'Prudence, shall we walk?')];

async function run(sc) {
    scenario = sc;
    current = freshModule();
    configValues = Object.assign({}, BASE_CONFIG, sc.config || {});
    for (const k of sc.unset || []) delete configValues[k];
    clock = sc.clock || CLOCK;
    rec = { queries: [], logs: [], events: [], notes: [], created: [], calls: [], timers: [], searches: [], reads: [], errors: [], handled: [], counts: {}, nth: {}, callIndex: 0 };
    const outcomes = [];
    for (const p of sc.payloads) {
        outcomes.push(await handle(p));
        for (let i = 0; i < 40; i++) await new Promise(r => process.nextTick(r));
    }
    if (sc.drain) {
        for (let i = 0; i < 5; i++) await new Promise(r => setImmediate(r));
    }
    const r = rec;
    return {
        name: sc.name, config: sc.config || {}, unset: sc.unset || [], rows: sc.rows || {}, rowsBy: sc.rowsBy || {},
        fail: sc.fail || {}, failNth: sc.failNth || {}, answers: sc.answers || [], notesIn: sc.notesIn || {},
        search: sc.search || null, searchError: sc.searchError || '', clock: sc.clock || CLOCK, unordered: !!sc.unordered,
        payloads: sc.payloads.map(p => JSON.stringify(p)), outcomes, handled: r.handled,
        queries: r.queries, logs: r.logs, events: r.events, notes: r.notes, created: r.created, calls: r.calls, timers: r.timers,
        searches: r.searches, reads: r.reads, errors: r.errors,
    };
}

async function main() {
    const out = {};

    out.context = [
        [{ agent: 'prudence' }, discussionRow()],
        [{ agent: 'prudence' }, discussionRow({ context: 'Last year we sold pies.', mode: 'async', id: 7 })],
        [{ agent: 'x' }, discussionRow({ context: '', topic: '' })],
    ].map(([a, d]) => ({ agent: a.agent, discussion: { id: d.id, topic: d.topic, context: d.context, mode: d.mode }, out: t.renderDiscussionContext(a, d) }));

    out.prompts = [
        { agent: agentRow('prudence'), discussion: discussionRow(), rag: '', soul: '', people: '', bootstrap: 'Be kind.' },
        { agent: agentRow('prudence', { startup_instructions: null }), discussion: discussionRow({ context: 'Ctx.' }), rag: '[a.md] remembered', soul: 'I am Prudence.', people: '## Your impressions of wendy\nKind.', bootstrap: '' },
        { agent: agentRow('prudence', { startup_instructions: '   ' }), discussion: discussionRow(), rag: '  ', soul: '\n', people: '', bootstrap: '  \n' },
    ].map(p => {
        configValues = Object.assign({}, BASE_CONFIG, { global_bootstrap: p.bootstrap });
        const s = t.buildSystemPrompt(p.agent, p.discussion, p.rag, p.soul, p.people);
        return { agent: p.agent.agent, instructions: p.agent.startup_instructions, discussion: { id: p.discussion.id, topic: p.discussion.topic, context: p.discussion.context, mode: p.discussion.mode }, rag: p.rag, soul: p.soul, people: p.people, bootstrap: p.bootstrap, static: s.static, dynamic: s.dynamic };
    });

    out.user = [
        [[], 'message', null],
        [[], 'vote-proposed', 'Bring pies?'],
        [[msg(1, 'wendy', 'Hi "all"\nnew line'), msg(2, 'ｗｅｎｄｙ', null), msg(3, 'prudence', '')], 'message', null],
        [[msg(1, 'wendy', 'Vote please')], 'vote-proposed', 'Bring pies? "yes"'],
        [[msg(1, 'wendy', 'Vote please')], 'vote-proposed', null],
        [[msg(1, 'wendy', 'Vote please')], 'vote-proposed', ''],
        [[msg(1, 'wendy', 'x')], 'message', 'ignored'],
        [[msg(1, 'home', '  😀')], 'discussion-active', null],
    ].map(([h, trig, q]) => ({ history: h, trigger: trig, question: q, out: t.buildUserMessage(h, trig, q) }));

    out.parse_vote = [
        '{"choice": 1, "reason": "Agreed"}', '{"choice":2}', 'I vote {"choice": 1, "reason": "yes"} because', '{"choice":"1"}', '{"choice": 1.5, "reason": ""}',
        '{"reason": {"a": 1}, "choice": 1}', '{"choice": 1, "reason": 0}', 'no json here', '{bad json}', '{}', '{"choice": -0}', '{"choice": 1e400}',
        '{"choice":1}{"choice":2}', '{\n"choice": 2,\n"reason": "No"\n}', '{"choice": null}', '{"choice": 3, "reason": ["x"]}', '',
        '{"choice": 1, "reason": "has } brace"}', '{"choice": 1, "reason": true}',
    ].map(text => ({ text, out: JSON.stringify(t.parseVoteResponse(text)) === undefined ? null : JSON.stringify(t.parseVoteResponse(text)) }));

    const scenarios = [
        { name: 'message broadcast', rows: baseRows({ 'discussion-history': [HISTORY] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [message()] },
        { name: 'message addressed', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(), answers: [answer('Gladly.')], payloads: [message()] },
        { name: 'message addressed by name token', rows: baseRows({ 'discussion-history': [[msg(90, 'wendy', 'Mr. CRANE, your thoughts?')]] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [message()] },
        { name: 'message addressed both', rows: baseRows({ 'discussion-history': [[msg(90, 'wendy', 'prudence and ezekiel: pies?')]] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [message()] },
        { name: 'name inside a word is not addressing', rows: baseRows({ 'discussion-history': [[msg(90, 'wendy', 'Imprudence cranes the neck.')]] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [message()] },
        { name: 'addressing skips an empty message', rows: baseRows({ 'discussion-history': [[msg(89, 'wendy', 'prudence?'), msg(90, 'home', '')]] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [message()] },
        { name: 'loop prevention', rows: baseRows({ 'discussion-history': [[msg(90, 'wendy', 'Hello'), msg(91, 'prudence', 'Hello back.')]] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [message()] },
        { name: 'loop prevention ignores other triggers', rows: baseRows({ 'discussion-history': [[msg(90, 'wendy', 'Hello'), msg(91, 'prudence', 'Hello back.')]] }), rowsBy: agentsBy({ 'ezekiel-crane': [] }), answers: [ANSWER], payloads: [active] },
        { name: 'discussion active empty history', rows: baseRows({ 'discussion-history': [[]], 'participants-load': [part('wendy'), part('prudence')] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [active] },
        { name: 'message empty history', rows: baseRows({ 'discussion-history': [[]], 'participants-load': [part('wendy'), part('prudence')] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [message()] },
        { name: 'participants not joined', rows: baseRows({ 'discussion-history': [HISTORY], 'participants-load': [part('wendy'), part('prudence', 'invited'), part('ezekiel-crane', 'left'), part('home', 'joined')] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [message()] },
        { name: 'no virtual participants', rows: baseRows({ 'participants-load': [part('wendy'), part('home')] }), rowsBy: agentsBy(), payloads: [message()] },
        { name: 'discussion not active', rows: baseRows({ 'discussion-load': [discussionRow({ status: 'concluded' })] }), rowsBy: agentsBy(), payloads: [message()] },
        { name: 'discussion not found', rows: baseRows({ 'discussion-load': [] }), payloads: [message()] },
        { name: 'missing discussion id', payloads: [{ type: 'virtual-agent', triggerType: 'message' }, { type: 'virtual-agent', discussionId: 0, triggerType: 'message' }] },
        { name: 'discussion id as a string', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [message('101')] },
        { name: 'no trigger type', rows: baseRows({ 'discussion-history': [[msg(90, 'wendy', 'Hello'), msg(91, 'prudence', 'Hi.')]] }), rowsBy: agentsBy({ 'ezekiel-crane': [] }), answers: [ANSWER], payloads: [{ type: 'virtual-agent', discussionId: 101 }] },
        { name: 'discussion load fails', fail: { 'discussion-load': 'db down' }, payloads: [message()] },
        { name: 'history load fails', rows: baseRows(), rowsBy: agentsBy(), fail: { 'discussion-history': 'history down' }, payloads: [message()] },
        { name: 'context and recall', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE], 'discussion-load': [discussionRow({ context: 'We sold pies last year.', mode: 'async' })] }), rowsBy: agentsBy(),
          notesIn: { 'context/soul': 'I am Prudence, the baker.', 'context/people/wendy': 'Wendy is kind.', 'context/people/ezekiel-crane': 'Ezekiel grumbles.' },
          search: { results: [{ similarity: 0.8, source_file: 'notes/pies.md', chunk_text: 'Pies sold out by noon.' }, { similarity: 0.2, source_file: 'x.md', chunk_text: 'low' }] },
          answers: [answer('Pies again, I think.')], payloads: [message()] },
        { name: 'recall search fails', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(), searchError: 'search down', answers: [ANSWER], payloads: [message()] },
        { name: 'coalesced burst reruns once', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE, ONLY_PRUDENCE, ONLY_PRUDENCE, [...ONLY_PRUDENCE, msg(93, 'wendy', 'Prudence, bring the cart too.')]] }), rowsBy: agentsBy(),
          answers: [Object.assign({}, answer('Gladly.'), { during: [message(), message()] }), answer('The cart as well.')], payloads: [message()] },
        { name: 'coalesced with no new message', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE, ONLY_PRUDENCE, ONLY_PRUDENCE, [...ONLY_PRUDENCE, msg(93, 'prudence', 'Gladly.')]] }), rowsBy: agentsBy(),
          answers: [Object.assign({}, answer('Gladly.'), { during: [message()] })], payloads: [message()] },
        { name: 'coalesced rerun is a message', rows: baseRows({ 'discussion-history': [[], [], [], [msg(93, 'wendy', 'Prudence?')], [msg(93, 'wendy', 'Prudence?')]], 'participants-load': [part('wendy'), part('prudence')] }), rowsBy: agentsBy(),
          answers: [Object.assign({}, answer('Hello all.'), { during: [message()] }), answer('Yes?')], payloads: [active] },
        { name: 'vote during a generation is not coalesced', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE], 'vote-question': [{ question: 'Bring pies?' }], 'participants-load': [part('wendy'), part('prudence')] }), rowsBy: agentsBy(),
          answers: [Object.assign({}, answer('Gladly.'), { during: [vote()] }), answer('{"choice": 1, "reason": "Pies sell."}')], payloads: [message()] },
        { name: 'two triggers in a row', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [message(), message()] },
        { name: 'vote valid', rows: baseRows({ 'discussion-history': [HISTORY], 'vote-question': [{ question: 'Bring pies?' }] }), rowsBy: agentsBy(),
          answers: [answer('{"choice": 1, "reason": "Pies sell."}')], payloads: [vote()] },
        { name: 'vote no reason', rows: baseRows({ 'discussion-history': [HISTORY], 'vote-question': [{ question: 'Bring pies?' }], 'participants-load': [part('wendy'), part('prudence')] }), rowsBy: agentsBy(),
          answers: [answer('Here: {"choice":2}')], payloads: [vote()] },
        { name: 'vote already cast', rows: baseRows({ 'discussion-history': [HISTORY], 'vote-question': [{ question: 'Bring pies?' }], 'ballot-check': [{ '?column?': 1 }], 'participants-load': [part('wendy'), part('prudence')] }), rowsBy: agentsBy(),
          answers: [answer('{"choice": 1, "reason": "Still yes."}')], payloads: [vote()] },
        { name: 'vote invalid choice', rows: baseRows({ 'discussion-history': [HISTORY], 'vote-question': [{ question: 'Bring pies?' }], 'participants-load': [part('wendy'), part('prudence')] }), rowsBy: agentsBy(),
          answers: [answer('{"choice": "yes", "reason": "Pies."}')], payloads: [vote()] },
        { name: 'vote unparseable', rows: baseRows({ 'discussion-history': [HISTORY], 'vote-question': [{ question: 'Bring pies?' }], 'participants-load': [part('wendy'), part('prudence')] }), rowsBy: agentsBy(),
          answers: [answer('I approve of pies.')], payloads: [vote()] },
        { name: 'vote empty reply', rows: baseRows({ 'discussion-history': [HISTORY], 'vote-question': [{ question: 'Bring pies?' }], 'participants-load': [part('wendy'), part('prudence')] }), rowsBy: agentsBy(),
          answers: [answer('  {"sender":"wendy","content":"yes"}')], payloads: [vote()] },
        { name: 'vote not found', rows: baseRows({ 'discussion-history': [HISTORY], 'vote-question': [], 'participants-load': [part('wendy'), part('prudence')] }), rowsBy: agentsBy(),
          answers: [answer('{"choice": 2, "reason": "No question."}')], payloads: [vote()] },
        { name: 'vote id as a string', rows: baseRows({ 'discussion-history': [HISTORY], 'vote-question': [{ question: 'Bring pies?' }], 'participants-load': [part('wendy'), part('prudence')] }), rowsBy: agentsBy(),
          answers: [answer('{"choice": 1, "reason": "ok"}')], payloads: [vote('55')] },
        { name: 'vote with no vote id', rows: baseRows({ 'discussion-history': [HISTORY], 'participants-load': [part('wendy'), part('prudence')] }), rowsBy: agentsBy(),
          answers: [answer('{"choice": 1, "reason": "ok"}')], payloads: [{ type: 'virtual-agent', discussionId: 101, triggerType: 'vote-proposed' }] },
        { name: 'vote skips pacing', config: { virtual_agent_response_delay_seconds: '5', virtual_agent_response_stagger_seconds: '3' }, rows: baseRows({ 'discussion-history': [HISTORY], 'vote-question': [{ question: 'Bring pies?' }] }), rowsBy: agentsBy(),
          answers: [answer('{"choice": 1, "reason": "yes"}')], payloads: [vote()] },
        { name: 'vote ballot insert fails', rows: baseRows({ 'discussion-history': [HISTORY], 'vote-question': [{ question: 'Bring pies?' }], 'participants-load': [part('wendy'), part('prudence')] }), rowsBy: agentsBy(),
          fail: { 'ballot-insert': 'invalid input syntax for type integer: "1.5"' }, answers: [answer('{"choice": 1.5, "reason": "half"}')], payloads: [vote()] },
        { name: 'delay and stagger', config: { virtual_agent_response_delay_seconds: '5', virtual_agent_response_stagger_seconds: '3' }, rows: baseRows({ 'discussion-history': [HISTORY] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [message()] },
        { name: 'three agents stagger', config: { virtual_agent_response_stagger_seconds: '2' }, rows: baseRows({ 'discussion-history': [HISTORY], 'participants-load': [part('wendy'), part('prudence'), part('ezekiel-crane'), part('home')] }),
          rowsBy: agentsBy({ home: [agentRow('home')] }), answers: [ANSWER], payloads: [message()] },
        { name: 'pacing settings unreadable', config: { virtual_agent_response_delay_seconds: 'soon', virtual_agent_response_stagger_seconds: '-2' }, rows: baseRows({ 'discussion-history': [HISTORY] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [message()] },
        { name: 'pacing setting fraction', config: { virtual_agent_response_delay_seconds: '1.9', virtual_agent_response_stagger_seconds: '0.5' }, rows: baseRows({ 'discussion-history': [HISTORY] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [message()] },
        { name: 'rate limited', config: { virtual_agent_rate_limit: '1' }, rows: baseRows({ 'discussion-history': [HISTORY], 'participants-load': [part('wendy'), part('prudence')] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [message(), active] },
        { name: 'over budget', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE], 'cost-sums': [{ cost_today: '1.5', cost_monthly: '2' }] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [message()] },
        { name: 'no api key', rows: baseRows({ 'discussion-history': [HISTORY] }), rowsBy: agentsBy({ prudence: [agentRow('prudence', { api_key: '' })], 'ezekiel-crane': [agentRow('ezekiel-crane', { provider: null })] }), answers: [ANSWER], payloads: [message()] },
        { name: 'post error fails', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy({ prudence: [agentRow('prudence', { model: '' })] }), fail: { 'chat-send': 'chat down' }, payloads: [message()] },
        { name: 'provider failure', rows: baseRows({ 'discussion-history': [HISTORY] }), rowsBy: agentsBy(), answers: [{ error: 'Anthropic API error 500: overloaded', status: 500 }, ANSWER], payloads: [message()] },
        { name: 'provider failure single agent', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(), answers: [{ error: 'Anthropic API error 500: overloaded', status: 500 }], payloads: [message()] },
        { name: 'malfunction post fails', rows: baseRows({ 'discussion-history': [HISTORY] }), rowsBy: agentsBy(), fail: { 'chat-send': 'chat down' }, answers: [{ error: 'Anthropic API error 500: overloaded', status: 500 }, ANSWER], payloads: [message()] },
        { name: 'leave fails', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(), fail: { 'discussion-leave': 'Agent is not a participant in this discussion' }, answers: [{ error: 'Anthropic API error 500: overloaded', status: 500 }], payloads: [message()] },
        { name: 'error status fails', rows: baseRows({ 'discussion-history': [HISTORY] }), rowsBy: agentsBy(), fail: { status: 'status down' }, answers: [{ error: 'Anthropic API error 500: overloaded', status: 500 }, ANSWER], payloads: [message()] },
        { name: 'failure usage and call rows fail', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(), fail: { 'usage-insert': 'insert failed', 'call-insert': 'call log down' }, answers: [{ error: 'API error 503: busy', status: 503 }], payloads: [message()] },
        { name: 'rerun history load fails', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(), failNth: { 'discussion-history': [null, null, null, 'history down'] },
          answers: [Object.assign({}, answer('Gladly.'), { during: [message()] })], payloads: [message()] },
        { name: 'stale configuration', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy({ prudence: [agentRow('prudence', { configuration: '{}' })] }), payloads: [message()] },
        { name: 'unsupported provider', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy({ prudence: [agentRow('prudence', { provider: 'unknown-provider', configuration: '{}' })] }), payloads: [message()] },
        { name: 'stop unsupported retries without stop', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(),
          answers: [{ error: 'OpenAI API error 400: Unsupported parameter: \'stop\'', status: 400 }, answer('Gladly.')], payloads: [message()] },
        { name: 'stop unsupported then failure', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(),
          answers: [{ error: 'API error 400: invalid stop sequences', status: 400 }, { error: 'API error 500: down', status: 500 }], payloads: [message()] },
        { name: 'impersonation cut', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(),
          answers: [answer('Gladly, let us walk.\nwendy: wonderful!\nprudence: yes')], payloads: [message()] },
        { name: 'impersonation json', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(),
          answers: [answer('Gladly.\n  {"sender" : "ezekiel-crane", "content": "me too"}')], payloads: [message()] },
        { name: 'empty reply skipped', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(), answers: [answer('   \n ')], payloads: [message()] },
        { name: 'whole reply impersonation', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(), answers: [answer('Prudence: I would love to.')], payloads: [message()] },
        { name: 'reply send fails', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(), failNth: { 'chat-send': ['chat down'] }, answers: [ANSWER], payloads: [message()] },
        { name: 'usage insert fails', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(), fail: { 'usage-insert': 'insert failed' }, answers: [ANSWER], payloads: [message()] },
        { name: 'no pricing', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy({ prudence: [agentRow('prudence', { model: 'not-a-model', configuration: '{}' })] }), answers: [ANSWER], payloads: [message()] },
        { name: 'learning on', config: { virtual_agent_learning_enabled: 'true' }, drain: true, unordered: true, rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(),
          answers: [answer('Gladly, I will bring three pies and the cart.'), answer('- Prudence bakes pies for the fair.')], payloads: [message()] },
        { name: 'learning skipped on a vote', config: { virtual_agent_learning_enabled: 'true' }, drain: true, rows: baseRows({ 'discussion-history': [HISTORY], 'vote-question': [{ question: 'Bring pies?' }], 'participants-load': [part('wendy'), part('prudence')] }), rowsBy: agentsBy(),
          answers: [answer('{"choice": 1, "reason": "Pies sell, and we have the flour already."}')], payloads: [vote()] },
        { name: 'transcript skipped by dream mode', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE], 'dream-mode': [{ dream_mode: 'none' }] }), rowsBy: agentsBy(), answers: [ANSWER], payloads: [message()] },
        { name: 'non-string reply text', rows: baseRows({ 'discussion-history': [ONLY_PRUDENCE] }), rowsBy: agentsBy(),
          answers: [{ result: { text: ['a', 'b'], tool_calls: [], usage: { input_tokens: 1, output_tokens: 1 }, finish_reason: 'stop', truncated: false } }], payloads: [message()] },
    ];
    out.scenarios = [];
    for (const sc of scenarios) {
        out.scenarios.push(await run(sc));
    }

    out.sql = {};
    for (const k of ['discussion-load', 'participants-load', 'discussion-history', 'vote-question', 'ballot-check', 'ballot-insert']) {
        out.sql[k] = sqlByKind[k];
    }
    fs.writeFileSync(path.join(__dirname, 'goldens_discussion.json'), JSON.stringify(out, null, 1) + '\n');
}

main().catch(err => { console.error(err); process.exit(1); });
