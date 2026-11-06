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
];

function kindOf(sql) {
    for (const [kind, re] of KINDS) {
        if (re.test(sql)) return kind;
    }
    throw new Error('unclassified query: ' + sql);
}

stub('db.js', {
    async query(sql, params) {
        const kind = kindOf(sql);
        rec.queries.push({ kind, params: (params || []).map(p => { const v = prepareValue(p); return v === undefined ? null : v; }) });
        const fail = (scenario.fail || {})[kind];
        if (fail) throw new Error(fail);
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
    logError() {},
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
    async readNote() { return null; },
});
stub('services/memory.js', { async searchMemory() { return []; } });
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
const src = fs.readFileSync(file, 'utf8') + '\nmodule.exports.__t = { buildProviderConf, parseBackoffCadence, totalDelayMsForCadence, buildExtractionPrompt, isRateLimited, recordCall, rateLimitResumeSeconds, retryWithBackoff, logCall, logTranscript, extractLearnings, withActivityIndicator, pingErroredAgents };\n';
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
    clock = sc.clock || 1793990000000;
    rec = { queries: [], logs: [], events: [], notes: [], created: [], calls: [], timers: [], counts: {}, callIndex: 0 };
    let out;
    try {
        out = outcome(await fn());
    } catch (err) {
        out = { error: err.message };
    }
    // Let fire-and-forget work (logCall, the spinner's off-switch) finish.
    for (let i = 0; i < 20; i++) await new Promise(r => process.nextTick(r));
    const r = rec;
    return { name: sc.name, config: sc.config || {}, rows: sc.rows || {}, fail: sc.fail || {}, answers: sc.answers || [], clock: sc.clock || 1793990000000, outcome: out, queries: r.queries, logs: r.logs, events: r.events, notes: r.notes, created: r.created, calls: r.calls, timers: r.timers };
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

    fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify(out, null, 1) + '\n');
}

main().catch(err => { console.error(err); process.exit(1); });
