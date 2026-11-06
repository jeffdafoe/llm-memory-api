// Writes goldens_chat.json beside this file: what the Node server's
// virtual-agent service does on the direct-chat path (handleDirectChat and
// the functions only it uses), so the Go port's tests compare against Node
// itself. Run from the repository root, after `npm ci` in node/api:
//
//     node go/server/internal/virtualagent/testdata/goldens_chat.js
//
// Same approach as goldens.js: the database, config, logger, events,
// actors, notes, memory search, chat and the provider call are stubbed and
// recorded; nothing leaves the process.

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
    ['errored-scan', /WHERE agc\.virtual = TRUE AND ac\.status/],
    ['chat-history', /JOIN actors fa ON fa\.id = cmt\.from_actor_id\s+JOIN actors ta/],
    ['chat-ack', /UPDATE chat_messages SET acked_at = NOW\(\) WHERE id = \$1 AND to_actor_id/],
];

function kindOf(sql) {
    for (const [kind, re] of KINDS) {
        if (re.test(sql)) return kind;
    }
    throw new Error('unclassified query: ' + sql);
}

// record logs one query (or one chat send, kind "chat-send"), fails it as
// the scenario says (failNth: the nth call of a kind; null passes), and
// answers from the scenario's rows.
function record(kind, params) {
    // timer marks a query made from a setTimeout callback (the activity
    // spinner's off-switch), as goldens.js does.
    const q = { kind, params: (params || []).map(p => { const v = prepareValue(p); return v === undefined ? null : v; }) };
    if (inTimer) q.timer = true;
    rec.queries.push(q);
    const n = (rec.nth[kind] = (rec.nth[kind] || 0) + 1);
    const nth = (scenario.failNth || {})[kind] || [];
    if (n <= nth.length && nth[n - 1] !== null) throw new Error(nth[n - 1]);
    const fail = (scenario.fail || {})[kind];
    if (fail) throw new Error(fail);
    return answerRows(kind);
}

function answerRows(kind) {
        const rows = (scenario.rows || {})[kind];
        if (Array.isArray(rows) && rows.length > 0 && Array.isArray(rows[0])) {
            const n = (rec.counts[kind] = (rec.counts[kind] || 0) + 1);
            return { rows: JSON.parse(JSON.stringify(rows[Math.min(n, rows.length) - 1])) };
        }
        return { rows: JSON.parse(JSON.stringify(rows || [])) };
}

stub('db.js', {
    async query(sql, params) {
        return record(kindOf(sql), params);
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
const ACTOR_IDS = { va: 7, other: 8, router: 9, 'salem-engine': 10, 'salem-magistrate': 11, 'salem-visitor': 12, human: 13 };
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
    // A scenario's notes are {slug: content}; any other slug is not found.
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
// chatSend is recorded as a "chat-send" query, so its place among the
// writes is compared: what chat.js read from its arguments, absent options
// as null or false.
stub('services/chat.js', {
    async chatSend(from, toAgents, discussionId, message, opts) {
        const o = opts || {};
        record('chat-send', [
            from, JSON.stringify(toAgents), message,
            o.sceneId === undefined ? null : o.sceneId,
            o.sceneStructure === undefined ? null : o.sceneStructure,
            o.conversationId === undefined ? null : o.conversationId,
            o.toolCalls ? JSON.stringify(o.toolCalls) : null,
            o.isError === true, o.ackOnInsert === true,
        ]);
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
const src = fs.readFileSync(file, 'utf8') + '\nmodule.exports.__t = { wrapBlock, stabilizeHistoryWindow, pruneSimHistory, paraphraseToolCall, formatGapMarker, buildCurrentTurnContext, buildToolUseMessages, trimHeadToUserMessage, extractCoLocatedNames, rememberSceneCoLocated, recallSceneCoLocated, forgetSceneCoLocated, collectDistinctSpeakers, scanForImpersonation, isStopUnsupportedError, stripSimToolInstructions, formatRelativeTime, buildDirectChatUserMessage, buildDirectChatSystemPrompt, buildSimChatSystemPrompt, buildSimCourtSystemPrompt, buildSimReflectionSystemPrompt, historyCapFor };\n';
function freshModule() {
    const m = new Module(file, module);
    m.filename = file;
    m.paths = Module._nodeModulePaths(path.dirname(file));
    m._compile(src, file);
    return m.exports;
}
const t = freshModule().__t;

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
    virtual_agent_chat_history_hours: '72',
    global_bootstrap: 'Be kind.',
};

const CLOCK = 1793990000000;
const iso = (msAgo) => new RealDate(CLOCK - msAgo).toISOString();
const MIN = 60 * 1000;
const HOUR = 60 * MIN;

function agentRow(over) {
    return Object.assign({
        actor_id: 7, agent: 'va', virtual: true, provider: 'anthropic', model: 'claude-haiku-4-5',
        api_key: 'enc', configuration: '{"_configVersion":1,"max_tokens":256}',
        startup_instructions: 'You are va.', personality: null, expertise: [],
        cost_budget_daily: null, cost_budget_monthly: null, cache_prompts: false,
        learning_enabled: true, max_tokens: null, temperature: null,
    }, over || {});
}

// A loadDirectChatHistory row, in its column order.
function row(from, to, message, msAgo, over) {
    return Object.assign({ from_agent: from, to_agent: to, message, sent_at: iso(msAgo), tool_calls: null, tool_call_id: null, tools_offered: null }, over || {});
}

const ANSWER = { result: { text: 'Hello there.', tool_calls: [], usage: { input_tokens: 120, output_tokens: 30 }, finish_reason: 'end_turn', truncated: false } };
const TOOL_ANSWER = { result: { text: '', tool_calls: [{ id: 'c9', name: 'speak', input: { text: 'Good day.' } }], usage: { input_tokens: 400, output_tokens: 20 }, finish_reason: 'tool_use', truncated: false } };
const TOOLS = [{ name: 'speak', description: 'Say something.', parameters: { type: 'object', properties: { text: { type: 'string' } } } }, { name: 'done', description: 'End the turn.', parameters: { type: 'object', properties: {} } }];
const SCENE = '11111111-2222-3333-4444-555555555555';
const SCENE2 = '66666666-7777-8888-9999-000000000000';

function sendRows(over) {
    return Object.assign({ 'load-agent': [agentRow()], 'cost-sums': [{ cost_today: '0', cost_monthly: '0' }], 'usage-insert': [{ id: 41 }], 'dream-mode': [] }, over || {});
}

function errInfo(err) {
    const o = { message: err.message };
    if (err.statusCode !== undefined) o.status_code = err.statusCode;
    if (err.code !== undefined) o.code = err.code;
    if (err.resumesInSeconds !== undefined) o.resumes_in_seconds = err.resumesInSeconds;
    return o;
}

async function run(sc, fn) {
    scenario = sc;
    configValues = Object.assign({}, BASE_CONFIG, sc.config || {});
    clock = sc.clock || CLOCK;
    rec = { queries: [], logs: [], events: [], notes: [], created: [], calls: [], timers: [], searches: [], reads: [], errors: [], counts: {}, nth: {}, callIndex: 0 };
    const outcomes = await fn();
    for (let i = 0; i < 40; i++) await new Promise(r => process.nextTick(r));
    for (const n of rec.notes) {
        if (n.content.length > 2000) n.content = { length: n.content.length, head: n.content.slice(0, 300), tail: n.content.slice(-300) };
    }
    const r = rec;
    return {
        name: sc.name, config: sc.config || {}, rows: sc.rows || {}, fail: sc.fail || {}, answers: sc.answers || [],
        failNth: sc.failNth || {}, notesIn: sc.notesIn || {}, search: sc.search || null, searchError: sc.searchError || '',
        clock: sc.clock || CLOCK, unordered: !!sc.unordered, outcomes,
        queries: r.queries, logs: r.logs, events: r.events, notes: r.notes, created: r.created, calls: r.calls, timers: r.timers,
        searches: r.searches, reads: r.reads, errors: r.errors,
    };
}

function tryJSON(fn) {
    try {
        const v = fn();
        return { ok: v === undefined ? null : JSON.stringify(v) };
    } catch (err) {
        return { error: err.message };
    }
}

async function main() {
    const out = {};

    out.wrap = [['T', 'p', 'D', '  body  '], ['T', 'p', 'D', ''], ['T', 'p', 'D', ' \n\t '], ['T', 'p', 'D', null], ['T', 'p', 'D', undefined], ['T', 'p', 'D', ' x﻿']]
        .map(([a, b, c, d]) => ({ args: [a, b, c, d === undefined ? '' : d], out: t.wrapBlock(a, b, c, d) }));

    out.paraphrase = [
        ['speak', { text: 'Hello "friend"\nbye' }], ['speak', { text: '' }], ['speak', { text: 5 }], ['speak', null],
        ['pay_with_item', { item: 'bread', seller: 'Anne', consume_now: true }], ['pay_with_item', { item: 'bread', consume_now: 'true', quote_id: 3 }],
        ['pay_with_item', { item: 'bread', quote_id: '2' }], ['pay_with_item', { item: 'bread', quote_id: 0 }], ['pay_with_item', { item: 'bread', quote_id: 'x' }],
        ['pay_with_item', { item: 'bread', quote_id: true }], ['pay_with_item', { item: 'bread', quote_id: [4] }], ['pay_with_item', { item: 'bread', quote_id: ' 1e1 ' }],
        ['pay_with_item', { item: 'bread', quote_id: { toString: 1 } }], ['pay_with_item', { item: '' }], ['pay_with_item', { seller: 'x' }],
        ['consume', { item: 'stew' }], ['consume', {}], ['move_to', { structure_name: 'the Inn' }], ['move_to', { structure_id: 'u-1' }], ['move_to', {}],
        ['take_break', { reason: 'tired' }], ['take_break', {}], ['take_break', 5], ['stop', null], ['chore', { type: 'sweep' }], ['chore', { type: 3 }],
        ['done', {}], ['look_around', {}], [7, {}], [undefined, {}], ['speak', 'text'], ['speak', [1]], ['move_to', { structure_name: 'Mill', structure_id: 'u' }],
    ].map(([name, input]) => ({ name: name === undefined ? null : name, name_undefined: name === undefined, input, out: tryJSON(() => t.paraphraseToolCall(name, input)) }));

    out.gap = [0, 9 * MIN, 10 * MIN, 12.4 * MIN, 12.5 * MIN, 117 * MIN, 2 * HOUR - 1, 2 * HOUR, 2.5 * HOUR, 49.5 * HOUR, -5 * HOUR, NaN]
        .map(ms => ({ ms: Number.isNaN(ms) ? null : ms, out: t.formatGapMarker(ms) }));

    out.colocated = [
        '## Around you\nhuddle: with Anne Hale, the keeper, a stranger, John, , Anne Hale',
        'huddle: (you are the only member)', 'huddle: not in a huddle', 'x\nhuddle: with The Mountain, the\tsmith, thereby',
        'xhuddle: with A', 'huddle: with ', 'huddle: with B\r\nhuddle: with C', 'huddle: with D more', '', 'a\rhuddle: with E',
        'huddle:  with F', 'huddle: with  G , ,H , I ',
    ].map(text => ({ text, out: t.extractCoLocatedNames(text) }));

    out.strip = [
        'You are a vendor. Use only the tools offered. When you have nothing more to add, say done.',
        'use only the TOOLS offered.   x', 'When you have nothing more to add,\n\nSAY DONE.\n', 'Nothing here.', '', 'Use only the tools offered.',
        'Use only the tools offeredX', 'when you have nothing more to add,say done.end',
    ].map(text => ({ text, out: t.stripSimToolInstructions(text) }));

    out.speakers = [[['wendy', 'home', 'wendy', ''], 'va'], [['ｗｅｎｄｙ', 'wendy'], ''], [[], 'va'], [[null, 'a'], null]]
        .map(([names, self]) => ({ names, self, out: [...t.collectDistinctSpeakers(names.map(n => ({ from_agent: n })), self)] }));

    out.impersonation = [
        ['Fine.\nwendy: no', ['wendy']], ['Fine.\r\n  WENDY : no', ['wendy']], ['ok\n{"sender": "x"}', []], ['ok\n  {"sender" :1}', ['a']],
        ['plain text', ['wendy']], ['', ['a']], ['first\nｗｅｎｄｙ: hi', ['wendy']], ['a.b: x', ['a.b']], ['axb: x', ['a.b']],
        ['x  \n\nhome:', ['home', 'va']], ['é\nÉcole: x', ['école']], ['line\rwendy: same line', ['wendy']], ['wendy: first', ['wendy']],
    ].map(([response, speakers]) => ({ response, speakers, out: t.scanForImpersonation(response, new Set(speakers)) }));

    out.stop_error = [
        'OpenRouter API error 400: unsupported parameter stop', 'API error 400: invalid "stop" value', 'error 500: stop_sequences',
        'error 422 stopSequences not allowed', 'error400 stop_sequences', 'error 404: not found', 'error 4xx stop',
    ].map(message => ({ message, out: t.isStopUnsupportedError(new Error(message)) }));

    // History transforms. Each case's rows go through every step.
    const tc = (id, name, input) => ({ id, name, input });
    const historyCases = [
        { name: 'companion pair', npc: 'va', rows: [row('human', 'va', 'Hi', 30 * MIN), row('va', 'human', 'Hello', 29 * MIN)] },
        { name: 'sim tick with tools', npc: 'va', rows: [
            row('salem-engine', 'va', 'You see Anne.', 3 * HOUR),
            row('va', 'salem-engine', '', 3 * HOUR - 1000, { tool_calls: [tc('c1', 'speak', { text: 'Morning, Anne.' })] }),
            row('salem-engine', 'va', '[OK] You spoke.', 3 * HOUR - 900, { tool_call_id: 'c1' }),
            row('salem-engine', 'va', 'Anne leaves.', 40 * MIN),
            row('salem-engine', 'va', 'Anne left. John arrives.', 39 * MIN),
            row('salem-engine', 'va', 'John waves.', 5 * MIN),
        ] },
        { name: 'parallel calls', npc: 'va', rows: [
            row('salem-engine', 'va', 'Perception.', 20 * MIN),
            row('va', 'salem-engine', 'Thinking aloud.', 19 * MIN, { tool_calls: [tc('a', 'speak', { text: 'Hi' }), tc('b', 'move_to', { structure_name: 'Mill' }), tc('c', 'done', {})] }),
            row('salem-engine', 'va', '[done]', 19 * MIN, { tool_call_id: 'c' }),
            row('salem-engine', 'va', '[OK] spoke', 19 * MIN, { tool_call_id: 'a' }),
            row('salem-engine', 'va', '[stray]', 19 * MIN, { tool_call_id: 'zz' }),
            row('salem-engine', 'va', 'Next.', 1 * MIN),
            row('va', 'salem-engine', '', 50 * 1000, { tool_calls: [tc('d', 'pay_with_item', { item: 'bread', quote_id: 1 }), tc('d', 'consume', { item: 'bread' })] }),
            row('salem-engine', 'va', '[ok]', 40 * 1000, { tool_call_id: 'd' }),
        ] },
        { name: 'stored shapes', npc: 'VA', rows: [
            row('assistant-first', 'va', 'x', 10 * HOUR),
            row('va', 'x', null, 9 * HOUR, { tool_calls: '[{"id":"s1","name":"speak","input":"{\\"text\\":\\"raw\\"}"}]' }),
            row('x', 'va', null, 8 * HOUR, { tool_call_id: 's1' }),
            row('va', 'x', 'kept', 7 * HOUR, { tool_calls: [] }),
            row('va', 'x', '', 6 * HOUR, { tool_calls: [{ name: 'chore', input: null }, 5] }),
            row('SALEM-ENGINE', 'va', null, 5 * HOUR),
            row('va', 'x', '', 4 * HOUR, { tool_calls: '' }),
            row('va', 'x', '', 3 * HOUR, { tool_calls: [{ id: 'q', name: 'speak', input: { text: 'a' } }, { id: 'r', name: 'take_break', input: 0 }] }),
        ] },
        { name: 'head trimmed', npc: 'va', rows: [
            row('salem-engine', 'va', '[OK]', 10 * MIN, { tool_call_id: 'old' }),
            row('va', 'salem-engine', 'stale', 9 * MIN),
            row('salem-engine', 'va', 'Now.', 8 * MIN),
        ] },
        { name: 'bad tool_calls object', npc: 'va', rows: [row('va', 'x', '', 1 * MIN, { tool_calls: { id: 1 } })] },
        { name: 'bad tool_calls json null', npc: 'va', rows: [row('va', 'x', '', 1 * MIN, { tool_calls: 'null' })] },
        { name: 'bad tool_calls null element', npc: 'va', rows: [row('va', 'x', '', 1 * MIN, { tool_calls: [null] })] },
        { name: 'quote_id object', npc: 'va', rows: [row('va', 'x', '', 1 * MIN, { tool_calls: [{ id: 'k', name: 'pay_with_item', input: { item: 'a', quote_id: { toString: 2 } } }] })] },
    ];
    out.history = historyCases.map(c => {
        const pruned = t.pruneSimHistory(c.rows);
        return {
            name: c.name, npc: c.npc, rows: c.rows,
            pruned: pruned.map(r => c.rows.indexOf(r)),
            messages: tryJSON(() => t.trimHeadToUserMessage(t.buildToolUseMessages(c.rows, c.npc))),
            pruned_messages: tryJSON(() => t.trimHeadToUserMessage(t.buildToolUseMessages(pruned, c.npc))),
        };
    });

    // stabilizeHistoryWindow over windows at and under the cap.
    const windowRows = (n, startMsAgo, stepMs) => Array.from({ length: n }, (_, i) => row('salem-engine', 'va', 'm' + i, startMsAgo - i * stepMs));
    out.stabilize = [
        { name: 'under cap', rows: windowRows(49, 50 * MIN, MIN), cap: 50 },
        { name: 'at cap, cut to grid', rows: windowRows(50, 100 * MIN, MIN), cap: 50 },
        { name: 'at cap, floor binds', rows: windowRows(50, 100 * 1000, 1000), cap: 50 },
        { name: 'over cap', rows: windowRows(52, 200 * MIN, 3 * MIN), cap: 50 },
        { name: 'on the grid', rows: [row('a', 'b', 'x', 0, { sent_at: '2026-11-06T09:15:00.000Z' })].concat(windowRows(30, 10 * MIN, 1000)), cap: 31 },
    ].map(c => ({ name: c.name, rows: c.rows, cap: c.cap, kept: t.stabilizeHistoryWindow(c.rows, c.cap).length }));

    out.turn_context = [[true, 'notes', 'eph'], [true, '', 'eph'], [true, 'notes', null], [false, 'notes', 'eph'], [true, '  ', null], [false, '', null]]
        .map(([sim, people, eph]) => ({ sim, people, eph, out: t.buildCurrentTurnContext(sim, people, eph) }));

    clock = CLOCK;
    out.relative = [0, 59 * 1000, 60 * 1000, 61 * MIN, 59 * MIN, HOUR, 23 * HOUR, 24 * HOUR, 49 * HOUR, -5 * MIN, 2 * MIN + 999]
        .map(ago => ({ now: CLOCK, sent_at: iso(ago), out: t.formatRelativeTime(new RealDate(CLOCK - ago)) }));

    out.user_message = [
        { name: 'one row', history: [row('human', 'va', 'Hi', 0)], from: 'human', latest: 'Hi' },
        { name: 'no rows, undefined', history: [], from: 'salem-engine', latest: undefined },
        { name: 'gaps', history: [row('human', 'va', 'a', 30 * HOUR), row('va', 'human', 'b', 29 * HOUR), row('human', 'va', null, 26.5 * HOUR), row('human', 'va', 'd', 2 * MIN)], from: 'human', latest: 'd' },
    ].map(c => ({ ...c, latest: c.latest === undefined ? null : c.latest, latest_undefined: c.latest === undefined, out: t.buildDirectChatUserMessage(c.history, c.from, c.latest) }));

    configValues = Object.assign({}, BASE_CONFIG);
    const promptAgents = [agentRow(), agentRow({ personality: 'Gruff.', startup_instructions: 'Sell wool. Use only the tools offered. When you have nothing more to add, say done.' }), agentRow({ startup_instructions: null, agent: 'salem-magistrate' })];
    out.prompts = [];
    for (const a of promptAgents) {
        for (const [rag, soul, people] of [['', '', ''], ['[n] fact', 'I am.', '## Your impressions of Anne\n\nkind']]) {
            out.prompts.push({
                agent: a, rag, soul, people,
                direct: t.buildDirectChatSystemPrompt(a, rag, soul, people),
                sim: t.buildSimChatSystemPrompt(a, rag, soul, people),
                reflection: t.buildSimReflectionSystemPrompt(a, rag, soul, people),
                court: t.buildSimCourtSystemPrompt(a, people),
            });
        }
    }
    configValues = Object.assign({}, BASE_CONFIG, { global_bootstrap: '' });
    out.prompts.push({ agent: agentRow(), rag: '', soul: '', people: '', bootstrap: '', direct: t.buildDirectChatSystemPrompt(agentRow(), '', '', ''), sim: t.buildSimChatSystemPrompt(agentRow(), '', '', ''), reflection: t.buildSimReflectionSystemPrompt(agentRow(), '', '', ''), court: t.buildSimCourtSystemPrompt(agentRow(), '') });
    out.caps = ['va', 'salem-magistrate', 'salem-visitor'].map(n => ({ agent: n, cap: t.historyCapFor(n) }));

    // The scene co-located cache: each op's result in order, on a fresh module.
    const cacheCases = [
        { name: 'remember recall forget', ops: [['recall', 's1'], ['remember', 's1', ['A', 'B']], ['recall', 's1'], ['remember', 's1', []], ['recall', 's1'], ['forget', 's1'], ['recall', 's1'], ['remember', null, ['A']], ['recall', null]] },
        { name: 'idle expiry', ops: [['remember', 's1', ['A']], ['advance', 10 * MIN], ['recall', 's1'], ['advance', 10 * MIN + 1], ['recall', 's1'], ['remember', 's2', ['B']], ['advance', 10 * MIN + 1], ['remember', 's3', ['C']], ['recall', 's2']] },
        { name: 'cap evicts oldest', ops: [['fill', 501], ['recall', 'f0'], ['recall', 'f1'], ['recall', 'f500']] },
        { name: 'cap after refresh', ops: [['fill', 500], ['advance', 1], ['recall', 'f0'], ['advance', 1], ['remember', 'new', ['N']], ['recall', 'f0'], ['recall', 'f1'], ['recall', 'new']] },
        { name: 'tie keeps insertion order', ops: [['fill-same', 501], ['recall', 'f0'], ['recall', 'f1'], ['recall', 'f500']] },
    ];
    out.cache = [];
    for (const c of cacheCases) {
        const ct = freshModule().__t;
        clock = CLOCK;
        const results = [];
        for (const [op, a, b] of c.ops) {
            if (op === 'recall') results.push(ct.recallSceneCoLocated(a));
            else if (op === 'remember') { ct.rememberSceneCoLocated(a, b); results.push('ok'); }
            else if (op === 'forget') { ct.forgetSceneCoLocated(a); results.push('ok'); }
            else if (op === 'advance') { clock += a; results.push('ok'); }
            else if (op === 'fill') { for (let i = 0; i < a; i++) { ct.rememberSceneCoLocated('f' + i, ['p' + i]); clock += 1; } results.push('ok'); }
            else if (op === 'fill-same') { for (let i = 0; i < a; i++) ct.rememberSceneCoLocated('f' + i, ['p' + i]); results.push('ok'); }
        }
        out.cache.push({ name: c.name, ops: c.ops, results });
    }

    // handleDirectChat end to end. Each scenario runs its inputs in order on
    // a fresh module (the limiter and the scene cache start empty).
    const companion = (message, over) => Object.assign({ agent: 'va', from: 'human', message, messageId: 501, opts: {} }, over || {});
    const sim = (message, opts, over) => Object.assign({ agent: 'va', from: 'salem-engine', message, messageId: 601, opts: Object.assign({ toolsOffered: TOOLS, sceneId: SCENE, sceneStructure: 'Inn', conversationId: 'conv-1', simActorId: 'actor-9', simActorName: 'Elias' }, opts) }, over || {});
    const companionHistory = [row('human', 'va', 'Earlier question about wool prices.', 30 * HOUR), row('va', 'human', 'They rose.', 29 * HOUR), row('human', 'va', 'ok', 1000)];
    const simHistory = [
        row('salem-engine', 'va', 'You stand in the Inn.', 3 * HOUR),
        row('va', 'salem-engine', '', 3 * HOUR - 1000, { tool_calls: [{ id: 'c1', name: 'speak', input: { text: 'Morning.' } }, { id: 'c2', name: 'done', input: {} }] }),
        row('salem-engine', 'va', '[OK] You spoke.', 3 * HOUR - 900, { tool_call_id: 'c1' }),
        row('salem-engine', 'va', '[done]', 3 * HOUR - 900, { tool_call_id: 'c2' }),
        row('salem-engine', 'va', 'Anne arrives.', 15 * MIN),
        row('salem-engine', 'va', 'Anne arrives. She nods.\nhuddle: with Anne Hale', 14 * MIN),
    ];
    const people = { 'context/soul': 'I keep the inn.', 'context/people/anne-hale': 'Anne pays late.', 'context/people/human': 'Asks about wool.' };
    const rag = { results: [{ source_file: 'notes/wool', chunk_text: 'Wool sells at 3 shillings.', similarity: 0.8 }, { source_file: 'notes/x', chunk_text: 'noise', similarity: 0.2 }] };
    const chatScenarios = [
        { name: 'companion with rag and history', rows: sendRows({ 'chat-history': [companionHistory.slice().reverse()] }), answers: [ANSWER], search: rag, notesIn: people, inputs: [companion('ok')] },
        { name: 'companion long message no rag', rows: sendRows({ 'chat-history': [[row('human', 'va', 'What does the market look like today?', 0)]] }), answers: [ANSWER], inputs: [companion('What does the market look like today?')] },
        { name: 'history hours not a number', config: { virtual_agent_chat_history_hours: 'abc' }, rows: sendRows({ 'chat-history': [[]] }), answers: [ANSWER], inputs: [companion('hi')] },
        { name: 'history hours zero', config: { virtual_agent_chat_history_hours: '0' }, rows: sendRows({ 'chat-history': [[]] }), answers: [ANSWER], inputs: [companion('hi')] },
        { name: 'companion long message with history', rows: sendRows({ 'chat-history': [companionHistory.slice().reverse()] }), answers: [ANSWER], search: rag, inputs: [companion('What about the price of wool now?')] },
        { name: 'companion wait retries', config: { virtual_agent_retry_backoff: '5' }, rows: sendRows({ 'chat-history': [[]] }), answers: [{ error: 'API error 503: busy', status: 503 }, ANSWER], inputs: [companion('hello', { opts: { ackReplyOnInsert: true } })] },
        { name: 'sim tool turn with learning on', unordered: true, config: { virtual_agent_learning_enabled: 'true' }, rows: sendRows({ 'chat-history': [[]] }), answers: [{ result: { text: 'I think the inn is quiet tonight and the fire is low, so I will tend it now.', tool_calls: [{ id: 'k1', name: 'done', input: {} }], usage: { input_tokens: 400, output_tokens: 20 }, finish_reason: 'tool_use', truncated: false } }], inputs: [sim('A long and quiet evening at the inn.', {})] },
        { name: 'companion rag fails', rows: sendRows({ 'chat-history': [[]] }), answers: [ANSWER], searchError: 'search down', inputs: [companion('hi there')] },
        { name: 'companion personality and empty reply', rows: sendRows({ 'load-agent': [agentRow({ personality: 'Terse.' })], 'chat-history': [[]] }), answers: [{ result: { text: null, usage: { input_tokens: 5, output_tokens: 0 } } }], inputs: [companion('hello', { messageId: 0 })] },
        { name: 'companion learning', unordered: true, config: { virtual_agent_learning_enabled: 'true' }, rows: sendRows({ 'chat-history': [[]], 'usage-insert': [[{ id: 41 }], [{ id: 42 }]] }),
          answers: [{ result: { text: 'The market is busy with wool and grain traders this week.', tool_calls: [], usage: { input_tokens: 120, output_tokens: 30 }, finish_reason: 'end_turn', truncated: false } }, { result: { text: '- Market busy', tool_calls: [], usage: { input_tokens: 50, output_tokens: 5 }, finish_reason: 'end_turn', truncated: false } }],
          inputs: [companion('Tell me about the market this week, in some detail please.')] },
        { name: 'sim turn with tool history', rows: sendRows({ 'chat-history': [simHistory.slice().reverse()] }), answers: [TOOL_ANSWER], notesIn: people,
          inputs: [sim('Anne arrives. She nods.\nhuddle: with Anne Hale', { ephemeralContext: '## Around you\nhuddle: with Anne Hale\n\nWhat now?', stableContext: 'You are Elias, innkeeper.', ackReplyOnInsert: true })] },
        { name: 'sim co-located cache across follow-up', rows: sendRows({ 'chat-history': [simHistory.slice().reverse(), simHistory.slice().reverse().concat([]), []] }), answers: [TOOL_ANSWER], notesIn: people,
          inputs: [
              sim('A perception.', { ephemeralContext: 'huddle: with Anne Hale' }),
              sim(undefined, { isToolResultCall: true }),
              sim('Alone now.', {}),
              sim(undefined, { isToolResultCall: true }),
          ] },
        { name: 'sim scene scoped history', rows: sendRows({ 'chat-history': [[]] }), answers: [TOOL_ANSWER],
          inputs: [sim('Fresh scene.', { historyScope: 'scene' }), sim('No scene.', { historyScope: 'scene', sceneId: null })] },
        { name: 'sim reflection', rows: sendRows({ 'load-agent': [agentRow({ startup_instructions: 'Sell wool. Use only the tools offered. When you have nothing more to add, say done.' })], 'chat-history': [[row('salem-engine', 'va', 'Reflect on today.', 0)]] }),
          answers: [ANSWER], notesIn: people, inputs: [sim('Reflect on today.\nhuddle: with Anne Hale', { toolsOffered: undefined, sceneId: undefined })] },
        { name: 'court session', rows: sendRows({ 'load-agent': [agentRow({ agent: 'salem-magistrate', actor_id: 11 })], 'chat-history': [[row('salem-engine', 'salem-magistrate', 'Matter opens.', 1000)]] }), answers: [TOOL_ANSWER], notesIn: people,
          inputs: [sim('Matter opens.\nhuddle: with Anne Hale', { stableContext: 'Matter: a stolen ewe.', historyScope: 'scene' }, { agent: 'salem-magistrate' })] },
        { name: 'court at the cap', rows: sendRows({ 'load-agent': [agentRow({ agent: 'salem-magistrate', actor_id: 11 })], 'chat-history': [windowRows(500, 200 * MIN, 1000).map(r => Object.assign({}, r, { to_agent: 'salem-magistrate' })).reverse()] }), answers: [TOOL_ANSWER],
          inputs: [sim('Next.', { stableContext: 'Matter.', historyScope: 'scene' }, { agent: 'salem-magistrate' })] },
        { name: 'sim busy window at cap', rows: sendRows({ 'chat-history': [windowRows(50, 100 * MIN, MIN).reverse()] }), answers: [TOOL_ANSWER], inputs: [sim('Latest.', {})] },
        { name: 'sim stable context blank', rows: sendRows({ 'chat-history': [[]] }), answers: [TOOL_ANSWER], inputs: [sim('', { stableContext: '   ' })] },
        { name: 'companion with tools', rows: sendRows({ 'chat-history': [[row('human', 'va', 'use a tool', 0)]] }), answers: [TOOL_ANSWER], notesIn: people, inputs: [companion('use a tool', { opts: { toolsOffered: TOOLS, ephemeralContext: 'ctx' } })] },
        { name: 'wait sim tick provider error', rows: sendRows({ 'chat-history': [[]] }), answers: [{ error: 'Anthropic API error 500: boom', status: 500 }], inputs: [sim('Tick.', { ackReplyOnInsert: true })] },
        { name: 'non-wait retry then reply', config: { virtual_agent_retry_backoff: '300,600,3600' }, rows: sendRows({ 'chat-history': [[]] }), answers: [{ error: 'API error 503: busy', status: 503 }, ANSWER], inputs: [companion('hello')] },
        { name: 'retry first-failure chat fails', config: { virtual_agent_retry_backoff: '5' }, rows: sendRows({ 'chat-history': [[]] }), failNth: { 'chat-send': ['chat send failed'] }, answers: [{ error: 'API error 400: bad', status: 400 }, TOOL_ANSWER], inputs: [sim('Tick.', {})] },
        { name: 'retry exhausted', config: { virtual_agent_retry_backoff: '1,2' }, rows: sendRows({ 'chat-history': [[]] }), answers: [{ error: 'down', status: 503 }], inputs: [companion('hello')] },
        { name: 'rate limited', config: { virtual_agent_rate_limit: '1' }, rows: sendRows({ 'chat-history': [[]] }), answers: [ANSWER], inputs: [companion('one'), companion('two')] },
        { name: 'scene continuation passes the limit', config: { virtual_agent_rate_limit: '1' }, rows: sendRows({ 'chat-history': [[]] }), answers: [TOOL_ANSWER],
          inputs: [sim('one', {}), sim(undefined, { isToolResultCall: true }), sim('other scene', { sceneId: SCENE2 }), companion('human', { opts: { sceneId: SCENE } })] },
        { name: 'over budget', rows: sendRows({ 'cost-sums': [{ cost_today: '1.5', cost_monthly: '2' }], 'chat-history': [[]] }), inputs: [sim('Tick.', { ackReplyOnInsert: true })] },
        { name: 'rate limit chat fails', config: { virtual_agent_rate_limit: '0' }, rows: sendRows({ 'chat-history': [[]] }), failNth: { 'chat-send': ['chat send failed', 'chat send failed'] }, inputs: [companion('x')] },
        { name: 'missing config notify', rows: { 'load-agent': [agentRow({ api_key: '', model: null })] }, failNth: { 'chat-send': ['chat send failed'] }, inputs: [companion('x', { opts: { notifyUnanswerable: true } })] },
        { name: 'missing config quiet', rows: { 'load-agent': [agentRow({ provider: '' })] }, inputs: [companion('x')] },
        { name: 'not virtual', rows: { 'load-agent': [agentRow({ virtual: false })] }, inputs: [companion('x')] },
        { name: 'not found', rows: { 'load-agent': [] }, inputs: [companion('x')] },
        { name: 'load fails', fail: { 'load-agent': 'db down' }, inputs: [companion('x')] },
        { name: 'history fails', rows: sendRows(), fail: { 'chat-history': 'history down' }, inputs: [companion('x')] },
        { name: 'stale config', rows: sendRows({ 'load-agent': [agentRow({ configuration: '{}' })] }), inputs: [companion('x')] },
        { name: 'unknown sender', rows: sendRows(), inputs: [companion('x', { from: 'ghost' })] },
        { name: 'unsupported provider', rows: sendRows({ 'load-agent': [agentRow({ provider: 'unknown-provider', configuration: '{}' })], 'chat-history': [[]] }), inputs: [companion('x')] },
        { name: 'reply send fails', rows: sendRows({ 'chat-history': [[]] }), failNth: { 'chat-send': ['chat send failed'] }, answers: [TOOL_ANSWER], inputs: [sim('Tick.', { ackReplyOnInsert: true })] },
        { name: 'ack fails', rows: sendRows({ 'chat-history': [[]] }), fail: { 'chat-ack': 'ack failed' }, answers: [ANSWER], inputs: [companion('x')] },
        { name: 'usage insert fails', rows: sendRows({ 'chat-history': [[]] }), fail: { 'usage-insert': 'insert failed' }, answers: [ANSWER], inputs: [companion('x')] },
        { name: 'no pricing', rows: sendRows({ 'load-agent': [agentRow({ model: 'not-a-model', configuration: '{}' })], 'chat-history': [[]] }), answers: [ANSWER], inputs: [companion('x')] },
        { name: 'companion tool result no message', rows: sendRows({ 'chat-history': [[]] }), inputs: [companion(undefined, { opts: { isToolResultCall: true } })] },
        { name: 'transcript skipped sim dream mode', rows: sendRows({ 'chat-history': [[]], 'dream-mode': [{ dream_mode: 'sim' }] }), answers: [TOOL_ANSWER], inputs: [sim('x', {})] },
        { name: 'non-string reply text', rows: sendRows({ 'chat-history': [[]] }), answers: [{ result: { text: ['a', 'b'], tool_calls: [{ id: 't', name: 'done', input: {} }], usage: { input_tokens: 1, output_tokens: 1 }, finish_reason: 'stop', truncated: false } }], inputs: [sim('x', {})] },
        { name: 'openrouter served by', rows: sendRows({ 'load-agent': [agentRow({ provider: 'openrouter', model: 'deepseek/deepseek-v4-flash', configuration: '{}' })], 'chat-history': [[]] }),
          answers: [{ result: { text: 'ok', tool_calls: [], usage: { input_tokens: 1000, output_tokens: 200, cost: 0.00012345, served_by: 'Baidu' }, finish_reason: 'stop', truncated: false } }], inputs: [companion('x')] },
    ];
    out.chat = [];
    for (const sc of chatScenarios) {
        const m = freshModule();
        const res = await run(sc, async () => {
            const outcomes = [];
            for (const input of sc.inputs) {
                try {
                    const reply = await m.handleDirectChat(input.agent, input.from, input.message, input.messageId, input.opts);
                    outcomes.push({ ok: reply === null || reply === undefined ? null : JSON.stringify(reply) });
                } catch (err) {
                    outcomes.push({ error: errInfo(err) });
                }
                // Let each call's background work run before the next call.
                for (let i = 0; i < 40; i++) await new Promise(r => process.nextTick(r));
            }
            return outcomes;
        });
        res.inputs = sc.inputs.map(i => ({ ...i, message: i.message === undefined ? null : i.message, opts: JSON.stringify(i.opts) }));
        out.chat.push(res);
    }

    fs.writeFileSync(path.join(__dirname, 'goldens_chat.json'), JSON.stringify(out, null, 1) + '\n');
}

main().catch(err => { console.error(err); process.exit(1); });
