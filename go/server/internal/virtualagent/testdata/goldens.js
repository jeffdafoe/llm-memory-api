// Writes goldens.json beside this file: what the Node server's
// virtual-agent service does for a set of scenarios, so the Go port's tests
// compare against Node itself. Run from the repository root, after `npm ci`
// in node/api:
//
//     node go/server/internal/virtualagent/testdata/goldens.js
//
// The database, config, logger, events, actors, notes and the provider call
// are stubbed and recorded; nothing leaves the process. Each scenario
// records the queries (a kind and the parameters as node-postgres prepared
// them), the log lines, the broadcasts, the notes saved, the provider calls
// and the outcome.

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

// The recorder for the scenario being run.
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
    ['mail-load', /JOIN actors fa ON fa\.id = m\.from_actor_id/],
    ['mail-ack', /UPDATE mail SET acked_at = NOW\(\)/],
];

function kindOf(sql) {
    for (const [kind, re] of KINDS) {
        if (re.test(sql)) return kind;
    }
    throw new Error('unclassified query: ' + sql);
}

// record logs one query (or one mail send, kind "mail-send") and throws
// the scenario's failure for it: fail[kind] for every call, failNth[kind][n]
// for the nth (null lets that call through).
function record(kind, params) {
    rec.queries.push({ kind, params });
    const n = (rec.nth[kind] = (rec.nth[kind] || 0) + 1);
    const nth = ((scenario.failNth || {})[kind] || [])[n - 1];
    if (nth) throw new Error(nth);
    const fail = (scenario.fail || {})[kind];
    if (fail) throw new Error(fail);
}

stub('db.js', {
    async query(sql, params) {
        const kind = kindOf(sql);
        record(kind, (params || []).map(p => { const v = prepareValue(p); return v === undefined ? null : v; }));
        const rows = (scenario.rows || {})[kind];
        if (Array.isArray(rows) && rows.length > 0 && Array.isArray(rows[0])) {
            // A queue of answers, one per call; the last repeats.
            const n = (rec.counts[kind] = (rec.counts[kind] || 0) + 1);
            return { rows: rows[Math.min(n, rows.length) - 1] };
        }
        return { rows: rows || [] };
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
    logError(subsystem, action, opts) {
        const { agent, context, contextId, message, statusCode } = opts;
        rec.errors.push({ subsystem, action, details: JSON.stringify({ agent, context, contextId, message, statusCode }) });
    },
});
stub('services/events.js', { broadcast(event, data) { rec.events.push({ event, data: JSON.stringify(data) }); } });
const ACTOR_IDS = { va: 7, other: 8, router: 9 };
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
stub('services/mail.js', {
    async mailSend(toAgent, fromAgent, subject, body, inReplyTo) {
        record('mail-send', [toAgent, fromAgent, subject, body, inReplyTo]);
        return { id: 'sent' };
    },
});
stub('services/chat.js', { async chatSend() { return {}; } });
stub('services/system-handler.js', { register() {} });

const realProvider = require(path.join(api, 'services/provider.js'));
stub('services/provider.js', {
    ...realProvider,
    decryptApiKey: (k) => 'dec:' + k,
    createProvider(provider, model, apiKey, conf) {
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

// A fixed clock: Date.now and new Date() read it; scenarios advance it.
let clock = 0;
const RealDate = Date;
global.Date = class extends RealDate {
    constructor(...args) {
        if (args.length === 0) super(clock);
        else super(...args);
    }
    static now() { return clock; }
};
// setTimeout runs at once and records its delay (the retry sleeps, the
// activity spinner's off-switch).
global.setTimeout = (fn, ms) => { rec.timers.push(ms); Promise.resolve().then(fn); return { unref() {} }; };

const file = path.join(api, 'services/virtual-agent.js');
const src = fs.readFileSync(file, 'utf8') + '\nmodule.exports.__t = { buildMailSystemPrompt, buildMailUserMessage, formatDuration, missingConfigParts, buildProviderConf, parseBackoffCadence, totalDelayMsForCadence, buildExtractionPrompt, isRateLimited, recordCall, rateLimitResumeSeconds, retryWithBackoff, logCall, logTranscript, extractLearnings, withActivityIndicator, pingErroredAgents };\n';
const mod = new Module(file, module);
mod.filename = file;
mod.paths = Module._nodeModulePaths(path.dirname(file));
mod._compile(src, file);
const va = mod.exports;
const t = va.__t;

const BASE_CONFIG = {
    virtual_agent_rate_limit: '10',
    virtual_agent_rate_window_seconds: '60',
    virtual_agent_cooldown_seconds: '300',
    virtual_agent_default_daily_budget: '1.00',
    virtual_agent_default_monthly_budget: '10.00',
    virtual_agent_retry_backoff: '300,600,3600',
    virtual_agent_retry_backoff_client_error: '5,15,60',
    virtual_agent_learning_enabled: 'true',
    virtual_agent_learning_min_tokens: '20',
    virtual_agent_error_ping_interval: '15',
    global_bootstrap: '',
    people_context_preamble: 'Your impressions follow.',
};

// The agent row loadAgent returns, in its column order.
function agentRow(over) {
    return Object.assign({
        actor_id: 7, agent: 'va', virtual: true, provider: 'anthropic', model: 'claude-haiku-4-5',
        api_key: 'enc', configuration: '{"_configVersion":1,"max_tokens":256}',
        startup_instructions: 'You are va.', personality: null, expertise: [],
        cost_budget_daily: null, cost_budget_monthly: null, cache_prompts: false,
        learning_enabled: true, max_tokens: null, temperature: null,
    }, over || {});
}

const ANSWER = {
    result: {
        text: 'Hello there.', tool_calls: [],
        usage: { input_tokens: 120, output_tokens: 30 },
        finish_reason: 'end_turn', truncated: false,
    },
};

function outcome(v) {
    return { ok: v === undefined ? null : JSON.stringify(v) };
}

async function run(sc, fn) {
    scenario = sc;
    configValues = Object.assign({}, BASE_CONFIG, sc.config || {});
    for (const k of sc.unset || []) delete configValues[k];
    clock = sc.clock || 1793990000000;
    rec = { queries: [], logs: [], events: [], notes: [], created: [], calls: [], timers: [], reads: [], searches: [], errors: [], counts: {}, nth: {}, callIndex: 0 };
    let out;
    try {
        out = outcome(await fn());
    } catch (err) {
        out = { error: err.message };
    }
    // Let fire-and-forget work (logCall, the spinner's off-switch) finish.
    for (let i = 0; i < 20; i++) await new Promise(r => process.nextTick(r));
    // Mail's learning extraction and transcript take more turns than that;
    // nothing here waits on real I/O, so a few macrotask turns drain them.
    if (sc.drain) {
        for (let i = 0; i < 5; i++) await new Promise(r => setImmediate(r));
    }
    const r = rec;
    return { name: sc.name, config: sc.config || {}, unset: sc.unset || [], rows: sc.rows || {}, fail: sc.fail || {}, failNth: sc.failNth || {}, notesIn: sc.notesIn || {}, search: sc.search || null, searchError: sc.searchError || '', answers: sc.answers || [], clock: sc.clock || 1793990000000, outcome: out, queries: r.queries, logs: r.logs, events: r.events, notes: r.notes, created: r.created, calls: r.calls, timers: r.timers, reads: r.reads, searches: r.searches, errors: r.errors };
}

function conf(row) {
    try {
        return { ok: JSON.stringify(t.buildProviderConf(row)) };
    } catch (err) {
        return { error: err.message };
    }
}

async function main() {
    const out = {};

    out.provider_conf = [
        agentRow(),
        agentRow({ configuration: null }),
        agentRow({ configuration: '' }),
        agentRow({ configuration: 'not json' }),
        agentRow({ configuration: '{"_configVersion":2}' }),
        agentRow({ configuration: '{"_configVersion":"1"}' }),
        agentRow({ configuration: '{"_configVersion":1.0}' }),
        agentRow({ configuration: '{"_configVersion":0}' }),
        agentRow({ configuration: '{"_configVersion":{"a":1}}' }),
        agentRow({ configuration: 'null' }),
        agentRow({ configuration: '5', provider: 'nobody' }),
        agentRow({ configuration: 'null', provider: 'nobody' }),
        agentRow({ configuration: '{"_configVersion":1}', cache_prompts: true, max_tokens: 900, temperature: '0.70' }),
        agentRow({ configuration: '{"_configVersion":1,"cache_prompts":false,"max_tokens":5,"temperature":0.1}', cache_prompts: true, max_tokens: 900, temperature: '0.7' }),
        agentRow({ provider: 'openrouter', model: 'deepseek/deepseek-v4-flash', configuration: '{}' }),
        agentRow({ provider: 'unknown-provider', model: 'x', configuration: '{}' }),
        agentRow({ provider: null, model: null, configuration: '{"a":1}' }),
        agentRow({ provider: 'anthropic', model: 'not-a-model', configuration: '{}' }),
        agentRow({ provider: 'constructor', model: 'x', configuration: '{}' }),
    ].map(row => ({ row, out: conf(row) }));

    out.cadence = [
        ['300,600,3600', '60,600,3600'], ['', '60,600,3600'], ['  5 , 15,60 ', '5,15,60'], ['x,y', '5,15,60'],
        ['-5,10', '5'], ['1.9,2e3', '5'], ['0x10', '5'], ['7', '60,600,3600'], [',,', '5,15'], ['10abc', '5'],
    ].map(([v, f]) => {
        const c = t.parseBackoffCadence(v, f);
        return { value: v, fallback: f, out: c, total1: t.totalDelayMsForCadence(c, 1), total5: t.totalDelayMsForCadence(c, 5) };
    });

    // node-postgres's prepareValue over JSON values (query parameters).
    out.prepare = [
        '[1,2]', '[[1,null],["a\\"b\\\\c"]]', '[{"k":[1,"x"]},true,null,"q"]', '[1e400]', '1e400', '{"a":1e400}',
        '"str"', 'true', 'false', 'null', '[[]]', '[]', '-0', '[-0]', '0.1', '[1.5e-7]', '12345678901234567890',
        '[["a",["b",null]],[]]', '{"toPostgres":"x"}', '[{"a":"b\\"c"}]', '["\u00e9\ud83d\ude00"]',
    ].map(j => { const v = prepareValue(JSON.parse(j)); return { in: j, out: v === undefined ? null : v }; });

    out.extraction = [['discussion', 'the plan'], ['chat', 'other'], ['mail', ''], ['other', 'x']].map(([type, hint]) => ({ type, hint, out: t.buildExtractionPrompt(type, hint) }));

    out.limits = [
        [{ cost_budget_daily: null, cost_budget_monthly: null }, {}],
        [{ cost_budget_daily: '2.50', cost_budget_monthly: '20.00' }, {}],
        [{ cost_budget_daily: null, cost_budget_monthly: null }, { virtual_agent_default_daily_budget: '', virtual_agent_default_monthly_budget: 'abc' }],
        [{ cost_budget_daily: '0.00', cost_budget_monthly: null }, { virtual_agent_default_monthly_budget: '1e2' }],
    ].map(([row, cfg]) => {
        configValues = Object.assign({}, BASE_CONFIG, cfg);
        const l = va.resolveEffectiveLimits(row);
        return { row, config: cfg, daily: l.dailyLimit === null ? 'null' : String(l.dailyLimit), monthly: l.monthlyLimit === null ? 'null' : String(l.monthlyLimit) };
    });

    // Rate limiter sequences. Each op's result is recorded in order.
    const limiterScenarios = [
        { name: 'limit trips and cools down', config: { virtual_agent_rate_limit: '2', virtual_agent_cooldown_seconds: '30' },
          ops: [['limited'], ['record'], ['limited'], ['record'], ['limited'], ['resume'], ['advance', 10000], ['limited'], ['resume'], ['advance', 25000], ['limited'], ['advance', 61000], ['limited'], ['resume']] },
        { name: 'scene continuation bypasses', config: { virtual_agent_rate_limit: '1' },
          ops: [['limited', 's1'], ['record', 's1'], ['limited', 's1'], ['record', 's1'], ['limited', 's2'], ['limited', 's1'], ['advance', 61000], ['limited', 's1'], ['record', 's1'], ['limited', 's1']] },
        { name: 'window boundary', config: { virtual_agent_rate_limit: '1' },
          ops: [['record', 's1'], ['advance', 59000], ['record', 's2'], ['advance', 1000], ['limited', 's1'], ['advance', 1], ['limited', 's1'], ['limited', 's2']] },
        { name: 'history boundary', config: { virtual_agent_rate_limit: '1' }, ops: [['record'], ['advance', 60000], ['limited'], ['resume']] },
        { name: 'per-agent overrides', rows: { 'rate-overrides': [{ configuration: '{"rate_limit":"1","rate_window_seconds":5,"cooldown_seconds":"2"}' }] },
          ops: [['effective'], ['limited'], ['record'], ['limited'], ['resume'], ['advance', 2500], ['limited'], ['effective']] },
        { name: 'override cache expires', rows: { 'rate-overrides': [[{ configuration: '{"rate_limit":3}' }], [{ configuration: '{"cooldown_seconds":1}' }]] },
          ops: [['effective'], ['advance', 30000], ['effective'], ['advance', 31000], ['effective']] },
        { name: 'garbage settings', config: { virtual_agent_rate_limit: 'x', virtual_agent_cooldown_seconds: 'y' }, rows: { 'rate-overrides': [{ configuration: '{"rate_window_seconds":[1,2]}' }] },
          ops: [['effective'], ['limited'], ['record'], ['limited']] },
        { name: 'override read fails', fail: { 'rate-overrides': 'db down' }, ops: [['effective'], ['effective']] },
        { name: 'override config null', rows: { 'rate-overrides': [{ configuration: 'null' }] }, ops: [['effective']] },
        { name: 'override config not an object', rows: { 'rate-overrides': [{ configuration: '"str"' }] }, ops: [['effective']] },
        { name: 'no window setting', config: { virtual_agent_rate_window_seconds: undefined }, ops: [['record', 's1']] },
    ];
    out.limiter = [];
    for (const sc of limiterScenarios) {
        // A fresh module per scenario, so the limiter's maps start empty.
        const m = new Module(file, module);
        m.filename = file;
        m.paths = mod.paths;
        m._compile(src, file);
        const lt = m.exports.__t;
        const results = [];
        const res = await run(sc, async () => {
            if (sc.config && 'virtual_agent_rate_window_seconds' in sc.config && sc.config.virtual_agent_rate_window_seconds === undefined) {
                delete configValues.virtual_agent_rate_window_seconds;
            }
            for (const [op, arg] of sc.ops) {
                try {
                    if (op === 'limited') results.push(await lt.isRateLimited('va', arg || null));
                    else if (op === 'record') { lt.recordCall('va', arg || null); results.push('recorded'); }
                    else if (op === 'resume') results.push(lt.rateLimitResumeSeconds('va'));
                    else if (op === 'advance') { clock += arg; results.push('advanced'); }
                    else if (op === 'effective') results.push(JSON.stringify(await m.exports.effectiveRateLimit('va')));
                } catch (err) {
                    results.push('error: ' + err.message);
                }
            }
        });
        res.ops = sc.ops;
        res.results = results;
        out.limiter.push(res);
    }

    // invokeAgent end to end. A fresh module per scenario resets the limiter.
    const invokeScenarios = [
        { name: 'success', rows: { 'load-agent': [agentRow()], 'cost-sums': [{ cost_today: '0', cost_monthly: '0' }], 'usage-insert': [{ id: 41 }] },
          answers: [ANSWER], opts: { userMessage: 'Hi', context: 'tick' } },
        { name: 'messages tools override skips', rows: { 'load-agent': [agentRow({ startup_instructions: null })], 'usage-insert': [{ id: 42 }] },
          answers: [{ result: { text: '', tool_calls: [{ id: 't1', name: 'speak', input: { text: 'x' } }], usage: { input_tokens: 0, output_tokens: 5, cache_creation_input_tokens: 10, cache_read_input_tokens: 20 }, finish_reason: 'tool_use', truncated: false } }],
          opts: { systemPrompt: 'Override.', messages: [{ role: 'user', content: 'a' }], tools: [{ name: 'speak', description: 'd', parameters: {} }], skipRateLimit: true, skipCostLimit: true } },
        { name: 'empty tools and messages arrays', rows: { 'load-agent': [agentRow()], 'cost-sums': [{ cost_today: '0', cost_monthly: '0' }], 'usage-insert': [{ id: 43 }] },
          answers: [ANSWER], opts: { userMessage: 'Hi', messages: [], tools: [] } },
        { name: 'provider error', rows: { 'load-agent': [agentRow()], 'cost-sums': [{ cost_today: '0', cost_monthly: '0' }], 'usage-insert': [{ id: 44 }] },
          answers: [{ error: 'Anthropic API error 500: boom', status: 500 }], opts: { userMessage: 'Hi', context: 'dream' } },
        { name: 'provider error usage insert fails', rows: { 'load-agent': [agentRow()], 'cost-sums': [{ cost_today: '0', cost_monthly: '0' }] }, fail: { 'usage-insert': 'insert failed' },
          answers: [{ error: 'network down' }], opts: { userMessage: 'Hi' } },
        { name: 'no message', opts: {} },
        { name: 'messages without user message', rows: { 'load-agent': [agentRow()], 'cost-sums': [{ cost_today: '0', cost_monthly: '0' }], 'usage-insert': [{ id: 45 }] },
          answers: [ANSWER], opts: { messages: [{ role: 'user', content: 'a' }] } },
        { name: 'agent not found', rows: { 'load-agent': [] }, opts: { userMessage: 'Hi' } },
        { name: 'missing api key', rows: { 'load-agent': [agentRow({ api_key: '' })] }, opts: { userMessage: 'Hi' } },
        { name: 'missing model', rows: { 'load-agent': [agentRow({ model: null })] }, opts: { userMessage: 'Hi' } },
        { name: 'over daily limit', rows: { 'load-agent': [agentRow({ cost_budget_daily: '0.50' })], 'cost-sums': [{ cost_today: '0.5012345', cost_monthly: '0.6' }] }, opts: { userMessage: 'Hi' } },
        { name: 'over monthly limit', rows: { 'load-agent': [agentRow()], 'cost-sums': [{ cost_today: '0.1', cost_monthly: '10.25' }] }, opts: { userMessage: 'Hi' } },
        { name: 'stale config', rows: { 'load-agent': [agentRow({ configuration: '{"_configVersion":0}' })], 'cost-sums': [{ cost_today: '0', cost_monthly: '0' }] }, opts: { userMessage: 'Hi' } },
        { name: 'rate limited second call', config: { virtual_agent_rate_limit: '1' }, rows: { 'load-agent': [agentRow()], 'cost-sums': [{ cost_today: '0', cost_monthly: '0' }], 'usage-insert': [{ id: 46 }] },
          answers: [ANSWER], opts: { userMessage: 'Hi' }, twice: true },
        { name: 'retry client error then success', rows: { 'load-agent': [agentRow()], 'cost-sums': [{ cost_today: '0', cost_monthly: '0' }], 'usage-insert': [{ id: 47 }] },
          answers: [{ error: 'bad request', status: 400 }, ANSWER], opts: { userMessage: 'Hi', skipRetry: false } },
        { name: 'retry exhausted', config: { virtual_agent_retry_backoff: '1,2' }, rows: { 'load-agent': [agentRow()], 'cost-sums': [{ cost_today: '0', cost_monthly: '0' }], 'usage-insert': [{ id: 48 }] },
          answers: [{ error: 'rate', status: 429 }, { error: 'server', status: 503 }, { error: 'last', status: 418 }], opts: { userMessage: 'Hi', skipRetry: false } },
        { name: 'no tool_calls no finish reason', rows: { 'load-agent': [agentRow()], 'cost-sums': [{ cost_today: '0', cost_monthly: '0' }], 'usage-insert': [{ id: 49 }] },
          answers: [{ result: { text: 'x', usage: { input_tokens: 1, output_tokens: 1 } } }], opts: { userMessage: 'Hi' } },
        { name: 'openrouter billed cost', rows: { 'load-agent': [agentRow({ provider: 'openrouter', model: 'deepseek/deepseek-v4-flash', configuration: '{}' })], 'cost-sums': [{ cost_today: '0', cost_monthly: '0' }], 'usage-insert': [{ id: 50 }] },
          answers: [{ result: { text: 'ok', tool_calls: [], usage: { input_tokens: 1000, output_tokens: 200, cost: 0.00012345, served_by: 'Baidu' }, finish_reason: 'stop', truncated: false } }], opts: { userMessage: 'Hi', context: 'tick' } },
        { name: 'no pricing', rows: { 'load-agent': [agentRow({ provider: 'anthropic', model: 'not-a-model', configuration: '{}' })], 'cost-sums': [{ cost_today: '0', cost_monthly: '0' }] },
          answers: [ANSWER], opts: { userMessage: 'Hi' } },
        { name: 'actor missing', rows: { 'load-agent': [agentRow({ agent: 'ghost' })], 'cost-sums': [{ cost_today: '0', cost_monthly: '0' }] }, answers: [ANSWER], agent: 'ghost', opts: { userMessage: 'Hi' } },
    ];
    out.invoke = [];
    for (const sc of invokeScenarios) {
        const m = new Module(file, module);
        m.filename = file;
        m.paths = mod.paths;
        m._compile(src, file);
        const res = await run(sc, async () => {
            const name = sc.agent || 'va';
            if (sc.twice) {
                await m.exports.invokeAgent(name, sc.opts);
            }
            return await m.exports.invokeAgent(name, sc.opts);
        });
        res.agent = sc.agent || 'va';
        res.opts = sc.opts;
        res.twice = !!sc.twice;
        out.invoke.push(res);
    }

    // logCall on its own, for the shapes invokeAgent does not produce.
    const callLogs = [
        { name: 'structured prompt', call: { actorId: 7, agentName: 'va', context: 'chat', contextId: 'abc', provider: 'anthropic', model: 'm', systemPrompt: { static: 'S', dynamic: 'D' }, userMessage: 'u', response: 'r', usage: { input_tokens: 3, output_tokens: 4, cache_read_input_tokens: 5, cache_creation_input_tokens: 6, served_by: 'Host' }, cost: 0.5, durationMs: 1234, usageId: 9, sceneId: '11111111-2222-3333-4444-555555555555', conversationId: 'c1', simActorId: 'a1', simActorName: 'Elias' } },
        { name: 'structured no static', call: { actorId: 7, agentName: 'va', provider: 'p', model: 'm', systemPrompt: { static: '', dynamic: 'D' } } },
        { name: 'no prompt', call: { actorId: 7, agentName: 'va', provider: 'p', model: 'm', response: '' } },
        { name: 'error with status', call: { actorId: 7, agentName: 'va', provider: 'p', model: 'm', systemPrompt: 'S', error: Object.assign(new Error('boom'), { status: 502 }), cost: 0 } },
        { name: 'error with statusCode', call: { actorId: 7, agentName: 'va', provider: 'p', model: 'm', systemPrompt: 'S', error: Object.assign(new Error('nf'), { statusCode: 404 }) } },
        { name: 'insert fails', fail: { 'call-insert': 'disk full' }, call: { actorId: 7, agentName: 'va', provider: 'p', model: 'm', systemPrompt: 'S' } },
    ];
    out.call_log = [];
    for (const sc of callLogs) {
        const res = await run(sc, () => t.logCall(sc.call));
        const c = Object.assign({}, sc.call);
        if (c.error) c.error = { message: c.error.message, status: c.error.status, statusCode: c.error.statusCode };
        res.call = c;
        out.call_log.push(res);
    }

    const big = 'x'.repeat(99500);
    const transcripts = [
        { name: 'plain', rows: { 'dream-mode': [{ dream_mode: 'conversation' }] }, args: ['va', 'System.', 'Hello', 'Reply.', { input_tokens: 10, output_tokens: 0 }, 'chat', { requestingAgent: 'other', model: 'm1' }] },
        { name: 'structured with discussion', rows: { 'dream-mode': [] }, args: ['va', { static: 'S', dynamic: 'RAG' }, 'U', 'R', null, 'discussion', { discussionId: 12 }] },
        { name: 'structured no static', rows: { 'dream-mode': [] }, args: ['va', { static: '', dynamic: 'RAG' }, 'U', 'R', undefined, 'mail', {}] },
        { name: 'skipped sim', rows: { 'dream-mode': [{ dream_mode: 'sim' }] }, args: ['va', 'S', 'U', 'R', null, 'chat', {}] },
        { name: 'skipped none', rows: { 'dream-mode': [{ dream_mode: 'none' }] }, args: ['va', 'S', 'U', 'R', null, 'chat', {}] },
        { name: 'truncate system', rows: { 'dream-mode': [] }, args: ['va', 'S'.repeat(5000), 'u'.repeat(97000), 'R', null, 'chat', {}] },
        { name: 'truncate both', rows: { 'dream-mode': [] }, args: ['va', 'S'.repeat(5000), big + 'y'.repeat(1000), 'R', null, 'chat', {}] },
        { name: 'non-string response', rows: { 'dream-mode': [] }, args: ['va', 'S', 'U', 42, { input_tokens: 'a' }, 'chat', {}] },
    ];
    out.transcript = [];
    for (const sc of transcripts) {
        const res = await run(sc, () => t.logTranscript(...sc.args));
        res.args = sc.args;
        // The big strings are rebuilt in the Go test.
        if (sc.name.startsWith('truncate')) res.args = null;
        for (const n of res.notes) {
            if (n.content.length > 2000) n.content = { length: n.content.length, head: n.content.slice(0, 300), tail: n.content.slice(-300) };
        }
        out.transcript.push(res);
    }

    const learnRow = agentRow();
    const learnings = [
        { name: 'saved', rows: { 'load-agent': [learnRow], 'usage-insert': [{ id: 60 }] }, answers: [{ result: { text: '- Fact one\n- Fact two', tool_calls: [], usage: { input_tokens: 50, output_tokens: 10 }, finish_reason: 'end_turn', truncated: false } }],
          args: [learnRow, 'You help.', 'Tell me about the build pipeline in detail please.', 'It builds in CI and ships a binary.', 'chat', 'other'] },
        { name: 'none', rows: { 'load-agent': [learnRow], 'usage-insert': [{ id: 61 }] }, answers: [{ result: { text: '  none ', tool_calls: [], usage: { input_tokens: 50, output_tokens: 1 }, finish_reason: 'end_turn', truncated: false } }],
          args: [learnRow, { static: 'S', dynamic: 'D' }, 'Tell me about the build pipeline in detail please.', 'It builds in CI and ships a binary.', 'discussion', 'the plan'] },
        { name: 'truncated', rows: { 'load-agent': [learnRow], 'usage-insert': [{ id: 62 }] }, answers: [{ result: { text: '- Fact', tool_calls: [], usage: { input_tokens: 50, output_tokens: 10 }, finish_reason: 'max_tokens', truncated: true } }],
          args: [learnRow, 'You help.', 'Tell me about the build pipeline in detail please.', 'It builds in CI and ships a binary.', 'mail', ''] },
        { name: 'too short', args: [learnRow, 'S', 'U', 'R', 'chat', 'other'] },
        { name: 'disabled globally', config: { virtual_agent_learning_enabled: 'false' }, args: [learnRow, 'S', 'U', 'R', 'chat', 'other'] },
        { name: 'disabled for agent', args: [agentRow({ learning_enabled: false }), 'S', 'U', 'R', 'chat', 'other'] },
        { name: 'non-string answer', rows: { 'load-agent': [learnRow], 'usage-insert': [{ id: 63 }] }, answers: [{ result: { text: 7, tool_calls: [], usage: { input_tokens: 50, output_tokens: 10 }, finish_reason: 'end_turn', truncated: false } }],
          args: [learnRow, 'You help.', 'Tell me about the build pipeline in detail please.', 'It builds in CI and ships a binary.', 'chat', 'other'] },
    ];
    out.learning = [];
    for (const sc of learnings) {
        const res = await run(sc, () => t.extractLearnings(...sc.args));
        res.args = sc.args;
        out.learning.push(res);
    }

    // retryWithBackoff with a first-failure callback.
    const retries = [
        { name: 'first failure callback', config: { virtual_agent_retry_backoff: '10,20,30', virtual_agent_retry_backoff_client_error: '1' }, results: ['fail:400', 'fail:500', 'ok'] },
        { name: 'callback throws', config: { virtual_agent_retry_backoff: '10' }, results: ['fail', 'ok'], callbackThrows: true },
        { name: 'status update fails', config: { virtual_agent_retry_backoff: '10' }, fail: { status: 'status down' }, results: ['fail', 'ok'] },
        { name: 'garbage cadence', config: { virtual_agent_retry_backoff: 'nope', virtual_agent_retry_backoff_client_error: '' }, results: ['fail:404', 'ok'] },
        { name: 'huge delay', config: { virtual_agent_retry_backoff: '3000000' }, results: ['fail', 'ok'] },
    ];
    out.retry = [];
    for (const sc of retries) {
        const seen = [];
        const res = await run(sc, async () => {
            let n = 0;
            return await t.retryWithBackoff('va', async () => {
                const r = sc.results[n++];
                if (r === 'ok') return 'done';
                const [, status] = r.split(':');
                const err = new Error('attempt ' + n);
                if (status) err.status = Number(status);
                throw err;
            }, async (err, info) => {
                seen.push({ error: err.message, info: JSON.stringify(info) });
                if (sc.callbackThrows) throw new Error('callback broke');
            });
        });
        res.results = sc.results;
        res.callback_throws = !!sc.callbackThrows;
        res.first_failure = seen;
        out.retry.push(res);
    }

    // The activity spinner.
    out.activity = [];
    for (const sc of [
        { name: 'fast call', result: 'v' },
        { name: 'slow call', result: 'v', advance: 5000 },
        { name: 'call throws', throws: 'inner' },
        { name: 'actor missing', agent: 'ghost' },
    ]) {
        const res = await run(sc, () => t.withActivityIndicator(sc.agent || 'va', async () => {
            if (sc.advance) clock += sc.advance;
            if (sc.throws) throw new Error(sc.throws);
            return sc.result;
        }));
        res.agent = sc.agent || 'va';
        res.advance = sc.advance || 0;
        res.throws = sc.throws || '';
        out.activity.push(res);
    }

    // The error ping: every errored agent gets a ping call.
    out.ping = [];
    for (const sc of [
        { name: 'recovers one, other still down', rows: { 'errored-scan': [{ agent: 'va' }, { agent: 'other' }], 'load-agent': [[agentRow()], []], 'usage-insert': [{ id: 70 }] }, answers: [{ result: { text: 'OK', tool_calls: [], usage: { input_tokens: 3, output_tokens: 1 }, finish_reason: 'end_turn', truncated: false } }] },
        { name: 'scan fails', fail: { 'errored-scan': 'no db' } },
    ]) {
        const m = new Module(file, module);
        m.filename = file;
        m.paths = mod.paths;
        m._compile(src, file);
        out.ping.push(await run(sc, () => m.exports.__t.pingErroredAgents()));
    }

    // The mail prompts and their helpers.
    out.mail_prompt = [
        { agent: agentRow({ personality: 'Dry and exact.' }), rag: '[notes/build] CI builds it.', soul: 'I am va.', people: '## Your impressions of other\n\nAsks well.', config: { global_bootstrap: 'Global rules.' } },
        { agent: agentRow({ startup_instructions: null }), rag: '', soul: '', people: '', config: {} },
        { agent: agentRow({ startup_instructions: '' }), rag: '', soul: '', people: 'P', config: { people_context_preamble: '' } },
        { agent: agentRow(), rag: 'R', soul: 'S', people: 'P', config: {} },
        { agent: agentRow(), rag: '', soul: '', people: 'P', config: {}, unset: ['people_context_preamble'] },
        { agent: agentRow(), rag: '', soul: '', people: '', config: {}, unset: ['global_bootstrap'] },
    ].map(c => {
        configValues = Object.assign({}, BASE_CONFIG, c.config);
        for (const k of c.unset || []) delete configValues[k];
        const res = Object.assign({}, c, { unset: c.unset || [] });
        try {
            const p = t.buildMailSystemPrompt(c.agent, c.rag, c.soul, c.people);
            res.out = { static: p.static, dynamic: p.dynamic };
        } catch (err) {
            res.error = err.message;
        }
        return res;
    });
    out.mail_user = [
        { from_agent: 'other', subject: 'Hi', body: 'Body\nline two' },
        { from_agent: 'a', subject: '', body: '' },
    ].map(m => ({ mail: m, out: t.buildMailUserMessage(m) }));
    out.duration = [0, 1, 59, 59.5, 60, 61, 80, 119, 120, 3599, 3600, 3601, 3660, 4210, 7200, 86399, 1000000].map(v => ({ in: v, out: t.formatDuration(v) }));
    const { personContextSlug } = require(path.join(api, 'services/people-slug.js'));
    out.slug = [
        'other', 'Josiah Thorne', '  Renée  O’Brien ', '../secrets', 'foo/bar', '!!!', '', '-a--b-', 'a  \t b', 'A B',
        'x﻿y', 'İstanbul', 'Kelvin', 'ﬁne', '½ cup', 'Straße', 'مرحبا', 'a - b', 'a!b c', '--',
        'Ź̧oe', 'Café\u0085x', 'ẞ',
    ].map(n => ({ in: n, out: personContextSlug(n) }));
    out.notice = [
        ['va', 'no-access', undefined], ['va', 'missing-config', ['model']], ['va', 'missing-config', ['API key', 'provider', 'model']], ['va', 'other', ['provider']],
    ].map(([a, r, m]) => ({ agent: a, reason: r, missing: m || null, out: va.unanswerableNotice(a, r, m) }));
    out.missing = [agentRow(), agentRow({ api_key: '', provider: null, model: '' }), agentRow({ model: null })].map(row => ({ row, out: t.missingConfigParts(row) }));

    // handleDirectMail end to end. A fresh module per scenario resets the
    // limiter.
    const MAIL_ID = 'a1b2c3d4-0000-4000-8000-000000000001';
    const MAIL_ROW = { subject: 'Build question', body: 'How does the build pipeline ship a binary?', from_agent: 'other' };
    const MAIL_ANSWER = { result: { text: 'It builds in CI and ships the binary to the VPS.', tool_calls: [], usage: { input_tokens: 300, output_tokens: 40 }, finish_reason: 'end_turn', truncated: false } };
    const LEARN_ANSWER = { result: { text: '- The build ships a binary.', tool_calls: [], usage: { input_tokens: 90, output_tokens: 8 }, finish_reason: 'end_turn', truncated: false } };
    const SEARCH = { results: [
        { source_file: 'notes/build', chunk_text: 'CI builds it.', similarity: 0.82 },
        { source_file: 'notes/other', chunk_text: 'Unrelated.', similarity: 0.2 },
        { source_file: 'notes/edge', chunk_text: 'Edge.', similarity: 0.3 },
        { source_file: 'notes/deploy', chunk_text: 'Deploy runs on the VPS.', similarity: 0.31 },
        { source_file: 'notes/none', chunk_text: 'No score.', similarity: null },
    ] };
    const quiet = agentRow({ learning_enabled: false });
    function mailRows(over) {
        return Object.assign({
            'load-agent': [quiet], 'mail-load': [MAIL_ROW], 'cost-sums': [{ cost_today: '0', cost_monthly: '0' }],
            'usage-insert': [{ id: 80 }], 'dream-mode': [{ dream_mode: 'none' }],
        }, over || {});
    }
    const reply = (text, extra) => ({ result: Object.assign({ text, tool_calls: [], usage: { input_tokens: 10, output_tokens: 1 }, finish_reason: 'end_turn', truncated: false }, extra || {}) });
    const mailScenarios = [
        { name: 'happy path', config: { global_bootstrap: 'Global rules.' },
          rows: mailRows({ 'load-agent': [agentRow({ personality: 'Dry and exact.' })], 'dream-mode': [{ dream_mode: 'conversation' }] }),
          search: SEARCH, notesIn: { 'context/soul': 'I am va, a careful builder.', 'context/people/other': 'Other asks good questions.' },
          answers: [MAIL_ANSWER, LEARN_ANSWER] },
        { name: 'bare prompt', rows: mailRows({ 'load-agent': [agentRow({ startup_instructions: null, learning_enabled: false })] }), answers: [MAIL_ANSWER] },
        { name: 'empty soul and people notes', rows: mailRows(), notesIn: { 'context/soul': '', 'context/people/other': '' }, answers: [MAIL_ANSWER] },
        { name: 'no preamble', config: { people_context_preamble: '' }, rows: mailRows(), notesIn: { 'context/people/other': 'Known.' }, answers: [MAIL_ANSWER] },
        { name: 'search fails', rows: mailRows(), searchError: 'embedding down', answers: [MAIL_ANSWER] },
        { name: 'sender with no slug', from: '!!!', rows: mailRows(), answers: [MAIL_ANSWER] },
        { name: 'sender display name', from: 'José Thorne', rows: mailRows({ 'mail-load': [Object.assign({}, MAIL_ROW, { from_agent: 'José Thorne' })] }),
          notesIn: { 'context/people/jose-thorne': 'A farmer.' }, answers: [MAIL_ANSWER] },
        { name: 're subject', rows: mailRows({ 'mail-load': [Object.assign({}, MAIL_ROW, { subject: 'Re: Build question' })] }), answers: [MAIL_ANSWER] },
        { name: 'retry then success', config: { virtual_agent_retry_backoff: '10,20' }, rows: mailRows(),
          answers: [{ error: 'Anthropic API error 500: boom', status: 500 }, MAIL_ANSWER] },
        { name: 'retry client error then success', rows: mailRows(), answers: [{ error: 'bad request', status: 400 }, MAIL_ANSWER] },
        { name: 'retry long backoff', config: { virtual_agent_retry_backoff: '3000,4000' }, rows: mailRows(), answers: [{ error: 'down' }, MAIL_ANSWER] },
        { name: 'retry exhausted', config: { virtual_agent_retry_backoff: '1,2' }, rows: mailRows(),
          answers: [{ error: 'rate', status: 429 }, { error: 'server', status: 503 }, { error: 'last', status: 502 }] },
        { name: 'retry notice fails', config: { virtual_agent_retry_backoff: '10' }, failNth: { 'mail-send': ['notice down'] }, rows: mailRows(),
          answers: [{ error: 'down' }, MAIL_ANSWER] },
        { name: 'rate limited', config: { virtual_agent_rate_limit: '0' }, rows: mailRows(), answers: [MAIL_ANSWER] },
        { name: 'rate limited notice fails', config: { virtual_agent_rate_limit: '0' }, failNth: { 'mail-send': ['send down'] }, rows: mailRows(), answers: [MAIL_ANSWER] },
        { name: 'over daily budget', rows: mailRows({ 'load-agent': [agentRow({ learning_enabled: false, cost_budget_daily: '0.50' })], 'cost-sums': [{ cost_today: '0.6', cost_monthly: '0.6' }] }), answers: [MAIL_ANSWER] },
        { name: 'missing api key', rows: mailRows({ 'load-agent': [agentRow({ api_key: null })] }) },
        { name: 'missing everything', rows: mailRows({ 'load-agent': [agentRow({ api_key: '', provider: null, model: null })], 'mail-load': [Object.assign({}, MAIL_ROW, { subject: 'Re: x' })] }) },
        { name: 'missing config notice fails', fail: { 'mail-send': 'send down' }, rows: mailRows({ 'load-agent': [agentRow({ model: null })] }) },
        { name: 'not virtual', rows: mailRows({ 'load-agent': [agentRow({ virtual: false })] }) },
        { name: 'agent not found', rows: mailRows({ 'load-agent': [] }) },
        { name: 'mail not found', rows: mailRows({ 'mail-load': [] }) },
        { name: 'mail load fails', fail: { 'mail-load': 'invalid input syntax for type uuid' }, rows: mailRows() },
        { name: 'agent load fails', fail: { 'load-agent': 'db down' }, rows: mailRows() },
        { name: 'empty reply', rows: mailRows(), answers: [reply('  \n ')] },
        { name: 'null reply', rows: mailRows(), answers: [reply(null)] },
        { name: 'non-string reply', rows: mailRows(), answers: [reply(7)] },
        { name: 'truncated reply', rows: mailRows({ 'load-agent': [agentRow()] }),
          answers: [reply('It builds in CI and ships', { usage: { input_tokens: 300, output_tokens: 256 }, finish_reason: 'max_tokens', truncated: true }), LEARN_ANSWER] },
        { name: 'stale config', rows: mailRows({ 'load-agent': [agentRow({ learning_enabled: false, configuration: '{"_configVersion":0}' })] }) },
        { name: 'reply send fails', failNth: { 'mail-send': ['reply down'] }, rows: mailRows(), answers: [MAIL_ANSWER] },
        { name: 'usage insert fails', fail: { 'usage-insert': 'insert failed' }, rows: mailRows(), answers: [MAIL_ANSWER] },
        { name: 'ack fails', fail: { 'mail-ack': 'ack failed' }, rows: mailRows(), answers: [MAIL_ANSWER] },
        { name: 'learning extraction fails', rows: mailRows({ 'load-agent': [agentRow()] }), answers: [MAIL_ANSWER, { error: 'learn down', status: 500 }] },
        { name: 'learning on transcript off', rows: mailRows({ 'load-agent': [agentRow()], 'dream-mode': [{ dream_mode: 'sim' }] }), answers: [MAIL_ANSWER, LEARN_ANSWER] },
        { name: 'transcript fails', fail: { 'dream-mode': 'dream read failed' }, rows: mailRows(), answers: [MAIL_ANSWER] },
        { name: 'transcript saved', rows: mailRows({ 'dream-mode': [{ dream_mode: 'conversation' }] }), answers: [MAIL_ANSWER] },
        { name: 'bootstrap key missing', unset: ['global_bootstrap'], rows: mailRows(), answers: [MAIL_ANSWER] },
        { name: 'openrouter agent', rows: mailRows({ 'load-agent': [agentRow({ learning_enabled: false, provider: 'openrouter', model: 'deepseek/deepseek-v4-flash', configuration: '{}' })] }),
          answers: [reply('Reply.', { usage: { input_tokens: 1000, output_tokens: 200, cost: 0.00012345, served_by: 'Baidu' }, finish_reason: 'stop' })] },
    ];
    out.mail = [];
    for (const sc of mailScenarios) {
        sc.drain = true;
        const m = new Module(file, module);
        m.filename = file;
        m.paths = mod.paths;
        m._compile(src, file);
        const res = await run(sc, () => m.exports.handleDirectMail(sc.agent || 'va', sc.from || 'other', MAIL_ID));
        res.agent = sc.agent || 'va';
        res.from = sc.from || 'other';
        res.mail_id = MAIL_ID;
        out.mail.push(res);
    }

    fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify(out, null, 1) + '\n');
}

main().catch(err => { console.error(err); process.exit(1); });
