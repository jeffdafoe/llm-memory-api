// Writes goldens.json beside this file: what the Node server's note
// enrichment does, so the Go port's tests compare against Node itself. Run
// from the repository root, after `npm ci` in node/api:
//
//     node go/server/internal/enrichment/testdata/goldens.js
//
// enrichment.js is loaded as it is, with the database, config, logger and
// the virtual-agent service (loadAgent, invokeAgent) stubbed and recorded,
// and a fixed clock. Each scenario calls enrichNote once and records one
// ordered stream: the agent lookups, the model calls with their options,
// the statements with their parameters as node-postgres prepared them, and
// the log lines; then whether the promise resolved or the error it rejected
// with. The metadata expression that save and the admin reindex put in
// front of enrichNote is read from both sources (they must be the same
// text) and run over jsonb values.

process.env.TZ = 'America/New_York';

const fs = require('fs');
const path = require('path');

const api = path.resolve(__dirname, '../../../../../node/api/src');
const { prepareValue } = require(require.resolve('pg/lib/utils', { paths: [api] }));

function stub(rel, exports) {
    const p = require.resolve(path.join(api, rel));
    require.cache[p] = { id: p, filename: p, loaded: true, exports };
}

// 2026-11-06T17:00:00.000Z: Date.now() and new Date() read it; new Date(x)
// is the real constructor.
const NOW = Date.UTC(2026, 10, 6, 17, 0, 0);
const RealDate = Date;
class FakeDate extends RealDate {
    constructor(...args) {
        if (args.length === 0) super(NOW);
        else super(...args);
    }
    static now() { return NOW; }
}
global.Date = FakeDate;

let sc;
let events;

stub('db.js', {
    async query(sql, params) {
        events.push({ kind: 'query', sql, params: (params || []).map((p) => { const v = prepareValue(p); return v === undefined ? null : v; }) });
        if (sc.updateError) throw new Error(sc.updateError);
        return { rows: [], rowCount: 1 };
    },
});
stub('services/config.js', {
    get(key) {
        if (!sc.config || !(key in sc.config)) throw new Error(`Config key "${key}" not found. Was config.init() called?`);
        return sc.config[key];
    },
});
stub('services/logger.js', {
    log(subsystem, action, details) { events.push({ kind: 'log', subsystem, action, details: JSON.stringify(details) }); },
});
stub('services/virtual-agent.js', {
    async loadAgent(name) {
        events.push({ kind: 'load', name });
        if (sc.loadError) throw new Error(sc.loadError);
        return sc.agent === undefined ? { provider: 'anthropic', model: 'claude-haiku-4-5', api_key: 'enc' } : sc.agent;
    },
    async invokeAgent(agent, opts) {
        events.push({
            kind: 'invoke', agent, keys: Object.keys(opts), user: opts.userMessage, context: opts.context,
            skipRateLimit: opts.skipRateLimit, skipCostLimit: opts.skipCostLimit, skipRetry: opts.skipRetry,
        });
        if (sc.invokeError) throw new Error(sc.invokeError);
        return {
            text: 'text' in sc ? sc.text : '{"cognitive_type":"semantic"}',
            tool_calls: [],
            usage: { input_tokens: 10, output_tokens: 5 },
            cost: 'cost' in sc ? sc.cost : 0.0012345678,
            finish_reason: 'stop',
            truncated: false,
        };
    },
});

const { enrichNote } = require(path.join(api, 'services/enrichment.js'));

const ON = { note_enrichment_enabled: 'true' };
const ago = (ms) => new RealDate(NOW - ms).toISOString();
const long = (n, ch) => ch.repeat(n);

// Each scenario: config, the enrichNote arguments (namespace, slug, title,
// content, metadata), the agent row (undefined: a complete one), and the
// model's text and cost.
const scenarios = [
    { name: 'gate off', config: { note_enrichment_enabled: 'false' } },
    { name: 'gate key missing', config: null },
    { name: 'gate TRUE is off', config: { note_enrichment_enabled: 'TRUE' } },
    { name: 'gate empty is off', config: { note_enrichment_enabled: '' } },
    { name: 'classified', config: ON },
    { name: 'skip conversation', config: ON, slug: 'conversations/2026-11-06-x' },
    { name: 'skip context', config: ON, slug: 'context/soul' },
    { name: 'skip dream', config: ON, slug: 'dreams/2026-11-06' },
    { name: 'kind is case sensitive', config: ON, slug: 'Conversations/x' },
    { name: 'reference kind', config: ON, slug: 'notes/codebase/x' },
    { name: 'instruction kind', config: ON, slug: 'instructions/bootstrap' },
    { name: 'task kind', config: ON, slug: 'tasks/x' },
    { name: 'learning kind', config: ON, slug: 'learnings/x' },
    { name: 'contexts is a note', config: ON, slug: 'contexts/x' },
    { name: 'null metadata', config: ON, meta: null },
    { name: 'metadata without stamp', config: ON, meta: { a: 1 } },
    { name: 'stamped 30 s ago', config: ON, meta: { _enriched_at: ago(30000) } },
    { name: 'stamped 59.999 s ago', config: ON, meta: { _enriched_at: ago(59999) } },
    { name: 'stamped 60 s ago', config: ON, meta: { _enriched_at: ago(60000) } },
    { name: 'stamped 2 minutes ago', config: ON, meta: { _enriched_at: ago(120000) } },
    { name: 'stamped in the future', config: ON, meta: { _enriched_at: ago(-3600000) } },
    { name: 'stamp not a date', config: ON, meta: { _enriched_at: 'yesterday' } },
    { name: 'stamp empty string', config: ON, meta: { _enriched_at: '' } },
    { name: 'stamp zero', config: ON, meta: { _enriched_at: 0 } },
    { name: 'stamp recent number', config: ON, meta: { _enriched_at: NOW - 1000 } },
    { name: 'stamp true', config: ON, meta: { _enriched_at: true } },
    { name: 'stamp array of a recent date', config: ON, meta: { _enriched_at: [ago(1000)] } },
    { name: 'stamp object', config: ON, meta: { _enriched_at: { a: 1 } } },
    { name: 'stamp object with toString value', config: ON, meta: { _enriched_at: { toString: 1 } } },
    { name: 'stamp local time recent', config: ON, meta: { _enriched_at: '2026-11-06T11:59:30' } },
    { name: 'stamp local time an hour off', config: ON, meta: { _enriched_at: '2026-11-06T12:59:30' } },
    { name: 'stamp date only', config: ON, meta: { _enriched_at: '2026-11-06' } },
    { name: 'metadata array', config: ON, meta: [1, 2] },
    { name: 'metadata string', config: ON, meta: 'x' },
    { name: 'metadata number', config: ON, meta: 5 },
    { name: 'metadata false', config: ON, meta: false },
    { name: 'no agent', config: ON, agent: null },
    { name: 'agent lookup fails', config: ON, loadError: 'connection refused' },
    { name: 'agent without provider', config: ON, agent: { provider: null, model: 'm', api_key: 'k' } },
    { name: 'agent without model', config: ON, agent: { provider: 'anthropic', model: '', api_key: 'k' } },
    { name: 'agent without key', config: ON, agent: { provider: 'anthropic', model: 'm', api_key: null } },
    { name: 'model call fails', config: ON, invokeError: 'Agent memory-enrichment over cost limit: Daily cost limit exceeded ($1.0000 / $1.00)' },
    { name: 'fenced json', config: ON, text: '```json\n{"cognitive_type":"episodic"}\n```' },
    { name: 'fenced no language', config: ON, text: '```\n{"cognitive_type":"reflective"}\n```' },
    { name: 'fenced one line', config: ON, text: '```{"cognitive_type":"procedural"}```' },
    { name: 'fenced JSON upper case', config: ON, text: '```JSON\n{"cognitive_type":"episodic"}\n```' },
    { name: 'fenced trailing text', config: ON, text: '```\n{"cognitive_type":"episodic"}\n```\nDone.' },
    { name: 'fenced unicode spaces', config: ON, text: '\u00a0```json\u2028\u3000{"cognitive_type":"episodic"}\u205f```\ufeff' },
    { name: 'fenced ideographic space', config: ON, text: '```json\u3000{"cognitive_type":"episodic"}\n```' },
    { name: 'fence only', config: ON, text: '``````' },
    { name: 'padded mixed case', config: ON, text: '  {"cognitive_type":"  Procedural\\t"}  ' },
    { name: 'dotted capital I', config: ON, text: '{"cognitive_type":"EP\u0130SODIC"}' },
    { name: 'capital sigma', config: ON, text: '{"cognitive_type":"SEMANTIC\u03a3"}' },
    { name: 'unknown type', config: ON, text: '{"cognitive_type":"other"}' },
    { name: 'type not a string', config: ON, text: '{"cognitive_type":7}' },
    { name: 'duplicate keys', config: ON, text: '{"cognitive_type":"bogus","cognitive_type":"episodic"}' },
    { name: 'array answer', config: ON, text: '[{"cognitive_type":"semantic"}]' },
    { name: 'string answer', config: ON, text: '"semantic"' },
    { name: 'not json', config: ON, text: 'It is semantic.' },
    { name: 'trailing comma', config: ON, text: '{"cognitive_type":"semantic",}' },
    { name: 'null answer', config: ON, text: 'null' },
    { name: 'empty text', config: ON, text: '' },
    { name: 'null text', config: ON, text: null },
    { name: 'zero text', config: ON, text: 0 },
    { name: 'number text', config: ON, text: 5 },
    { name: 'array text', config: ON, text: [{ type: 'text', text: '{"cognitive_type":"semantic"}' }] },
    { name: 'long unparsed preview', config: ON, text: long(250, 'x') },
    { name: 'preview of astral characters', config: ON, text: long(150, '\u{1F600}') },
    { name: 'preview cut through a pair', config: ON, text: 'x' + long(150, '\u{1F600}') },
    { name: 'metadata write fails', config: ON, updateError: 'relation "documents" does not exist' },
    { name: 'free cost', config: ON, cost: 0 },
    { name: 'tiny cost', config: ON, cost: 0.0000004 },
    { name: 'content at the limit', config: ON, content: long(4000, 'a') },
    { name: 'content past the limit', config: ON, content: long(4001, 'a') },
    { name: 'content astral at the limit', config: ON, content: long(2000, '\u{1F600}') },
    { name: 'content astral past the limit', config: ON, content: long(2001, '\u{1F600}') },
    { name: 'content cut through a pair', config: ON, content: 'a' + long(2000, '\u{1F600}') },
    { name: 'null title', config: ON, title: null },
    { name: 'mixed-case slug', config: ON, slug: 'Notes/Mixed' },
];

const metaInputs = ['null', '{"a": 1}', '[]', '[1]', '"x"', '"{\\"a\\": 1}"', '"null"', '""', '0', '5', 'true', 'false', '"5"', '" 7 "', '"\\"nested\\""', '{}'];

async function main() {
    const out = { scenarios: [], metadata: [] };
    for (const s of scenarios) {
        sc = s;
        events = [];
        const args = [
            s.namespace || 'agent-a',
            s.slug || 'notes/topic',
            'title' in s ? s.title : 'Topic',
            s.content || 'Body of the note.',
            'meta' in s ? s.meta : null,
        ];
        let result;
        try {
            await enrichNote(...args);
            result = { ok: true };
        } catch (e) {
            result = { error: e.message };
        }
        out.scenarios.push({
            name: s.name,
            config: s.config,
            namespace: args[0], slug: args[1], title: args[2], content: args[3], meta: JSON.stringify(args[4]),
            agent: s.agent === undefined ? { provider: 'anthropic', model: 'claude-haiku-4-5', api_key: 'enc' } : s.agent,
            loadError: s.loadError || null,
            invokeError: s.invokeError || null,
            updateError: s.updateError || null,
            text: JSON.stringify('text' in s ? s.text : '{"cognitive_type":"semantic"}'),
            cost: 'cost' in s ? s.cost : 0.0012345678,
            events,
            result,
        });
    }

    // documents.js saveNote and the admin reindex parse doc.metadata (as
    // node-postgres returned the jsonb) with the same expression.
    const read = (rel) => fs.readFileSync(path.join(api, rel), 'utf8');
    const pick = (src) => src.match(/var parsedMeta = (.+);\n/)[1];
    const expr = pick(read('services/documents.js'));
    if (pick(read('routes/admin.js')) !== expr) throw new Error('save and reindex parse metadata differently');
    const parse = new Function('doc', 'return ' + expr + ';');
    for (const raw of metaInputs) {
        try {
            const v = parse({ metadata: JSON.parse(raw) });
            out.metadata.push({ raw, ok: JSON.stringify(v) });
        } catch (e) {
            out.metadata.push({ raw, error: e.message });
        }
    }

    fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify(out, null, 1) + '\n');
}

main().catch((e) => { console.error(e); process.exit(1); });
