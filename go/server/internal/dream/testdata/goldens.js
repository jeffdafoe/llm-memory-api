// Writes goldens.json beside this file: what the Node server's dream job
// does, so the Go port's tests compare against Node itself. Run from the
// repository root, after `npm ci` in node/api:
//
//     node go/server/internal/dream/testdata/goldens.js
//
// dream.js is compiled with one extra export line for the helpers it does
// not export. The database (pool queries and the lock's checked-out
// client), config, logger, notes, invokeAgent, the trusted-creator check,
// node-cron and setTimeout are stubbed; nothing leaves the process. A pass
// scenario scripts the database's answers in order and records one ordered
// stream of events: each statement with its parameters as node-postgres
// prepared them, the lock connection's checkout and release, the
// invokeAgent calls, the notes saved, read and listed, the trust checks,
// the log lines, the error_log calls and the timer delays. Timestamps in a
// script are Postgres text and go through pg-types, as the driver read them.

process.env.TZ = 'UTC';

const fs = require('fs');
const path = require('path');
const Module = require('module');
const { EventEmitter } = require('events');

const api = path.resolve(__dirname, '../../../../../node/api/src');
const { prepareValue } = require(require.resolve('pg/lib/utils', { paths: [api] }));
const pgTypes = require(require.resolve('pg-types', { paths: [api] }));
const parseTimestamptz = pgTypes.getTypeParser(1184, 'text');

function stub(rel, exports) {
    const p = require.resolve(path.join(api, rel));
    require.cache[p] = { id: p, filename: p, loaded: true, exports };
}

// The scenario being run and its event stream.
let sc;
let events;

const DATE_COLUMNS = new Set(['last_dream_at', 'created_at', 'updated_at', 'min_created', 'min_updated', 'min_note_updated']);

let lockClient = null;

function answer(sql, params, onConn) {
    events.push({ kind: 'query', sql, params: (params || []).map((p) => { const v = prepareValue(p); return v === undefined ? null : v; }), conn: onConn });
    const step = sc.script.shift();
    if (!step) throw new Error('unscripted statement: ' + sql.trim().slice(0, 60));
    if (!sql.includes(step.match)) throw new Error(`statement does not match "${step.match}": ` + sql.trim().slice(0, 60));
    if (step.lose) {
        // The lock's session dies: pg emits 'error' on the checked-out client.
        lockClient.emit('error', new Error(step.lose));
    }
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

stub('db.js', {
    async query(sql, params) { return answer(sql, params, false); },
    async connect() {
        events.push({ kind: 'connect' });
        if (sc.connectError) throw new Error(sc.connectError);
        lockClient = new EventEmitter();
        lockClient.query = async (sql, params) => answer(sql, params, true);
        lockClient.release = (err) => events.push({ kind: 'release', destroy: !!err });
        return lockClient;
    },
});

stub('services/config.js', {
    get(key) {
        if (!(key in sc.config)) throw new Error(`Config key "${key}" not found. Was config.init() called?`);
        return sc.config[key];
    },
});
stub('services/logger.js', {
    log(subsystem, action, details) { events.push({ kind: 'log', subsystem, action, details: JSON.stringify(details) }); },
    logError(subsystem, action, opts) {
        const s = (v) => (v === undefined || v === null ? '' : String(v));
        events.push({ kind: 'error', subsystem, action, agent: s(opts.agent), context: s(opts.context), message: s(opts.message) });
    },
});

function nth(map, kind) {
    sc.counts[kind] = (sc.counts[kind] || 0) + 1;
    return ((map || {})[kind] || [])[sc.counts[kind] - 1];
}

stub('services/documents.js', {
    async saveNote(namespace, title, content, slug, createdBy, metadata, extension, opts) {
        events.push({ kind: 'save', namespace, title, content, slug, created_by: createdBy, upsert: !!(opts && opts.upsert) });
        const fail = nth(sc.failNth, 'save');
        if (fail) throw new Error(fail);
        return { id: 1 };
    },
    async readNote(namespace, slug) {
        events.push({ kind: 'read', namespace, slug });
        const notes = sc.notesIn || {};
        if (!(slug in notes)) throw Object.assign(new Error(`Note not found: ${slug}`), { statusCode: 404 });
        return { slug, content: notes[slug] };
    },
    async listNotes(namespace, limit, offset, prefix) {
        events.push({ kind: 'list', namespace, limit: JSON.stringify(limit), offset: JSON.stringify(offset), prefix });
        return { notes: ((sc.lists || {})[prefix] || []).map((slug) => ({ slug })) };
    },
});

stub('services/virtual-agent.js', {
    async invokeAgent(agent, opts) {
        events.push({
            kind: 'invoke', agent, user: opts.userMessage, context: opts.context,
            skipRateLimit: !!opts.skipRateLimit, skipCostLimit: !!opts.skipCostLimit, skipRetry: !!opts.skipRetry,
        });
        const a = sc.answers.shift();
        if (!a) throw new Error('no answer scripted');
        if (a.error) throw new Error(a.error);
        return {
            text: 'text' in a ? a.text : 'reply',
            tool_calls: [],
            usage: 'usage' in a ? a.usage : { input_tokens: 10, output_tokens: 5 },
            cost: 0,
            finish_reason: a.finish || 'stop',
            truncated: !!a.truncated,
        };
    },
});

stub('services/admin-permissions.js', {
    async isTrustedCreator(id) {
        // The real check is false for a null id; Go does not ask for one.
        if (id === null || id === undefined) return false;
        events.push({ kind: 'trust', id });
        if (sc.trustError) throw new Error(sc.trustError);
        return (sc.trusted || [1]).includes(id);
    },
});

// node-cron: the real validate; schedule hands back the callback.
const cronPath = require.resolve('node-cron', { paths: [path.join(api, 'services')] });
const realCron = require(cronPath);
let scheduled = null;
require.cache[cronPath].exports = {
    validate: realCron.validate,
    schedule: (expression, fn, options) => {
        const task = realCron.schedule(expression, fn, options);
        task.stop();
        scheduled = fn;
        return { stop() {} };
    },
};

// A fixed clock: Date.now and new Date() read it.
let clock = 0;
const RealDate = Date;
global.Date = class extends RealDate {
    constructor(...args) {
        if (args.length === 0) super(clock);
        else super(...args);
    }
    static now() { return clock; }
};
global.setTimeout = (fn, ms) => {
    events.push({ kind: 'sleep', ms });
    Promise.resolve().then(fn);
    return { unref() {} };
};

const file = path.join(api, 'services/dream.js');
const source = fs.readFileSync(file, 'utf8');
const src = source + '\nmodule.exports.__t = { SIGNAL_PATTERNS, REASONING_PREAMBLE_MARKERS, LEDGER_PRECEDENCE_DIRECTIVE, slugify, computeDailyChunks, ledgerLineSpeaker, nameMatcher, validatePersonSlug, LEDGER_LINE, JSON_ARRAY_HEAD };\n';
const mod = new Module(file, module);
mod.filename = file;
mod.paths = Module._nodeModulePaths(path.dirname(file));
mod._compile(src, file);
const dream = mod.exports;
const t = dream.__t;
const { personContextSlug } = require(path.join(api, 'services/people-slug.js'));
const distiller = require(path.join(api, 'services/sim-conversation-distiller.js'));
const { SOUL_LENGTH_DIRECTIVE } = require(path.join(api, 'services/sim-soul.js'));

// The title is computed inline in processDreamChunk; its two lines are read
// from the source, so the goldens run Node's own expressions.
const titleLines = source.split('\n').filter((l) => /const titleMatch = |const title = titleMatch/.test(l)).join('\n');
if (!titleLines.includes('titleMatch[1].trim()')) throw new Error('title lines not found in dream.js');
const titleOf = new Function('response', titleLines + '\nreturn title;');

// ---------------------------------------------------------------- pure

const out = {};

out.constants = {
    signals: t.SIGNAL_PATTERNS.map((r) => [r.source, r.flags]),
    markers: t.REASONING_PREAMBLE_MARKERS,
    ledgerDirective: t.LEDGER_PRECEDENCE_DIRECTIVE,
    soulLength: SOUL_LENGTH_DIRECTIVE,
};

const SIM_DAY = [
    '## Sim conversation day 2026-11-05',
    '[Thursday 09:00 Constance Scott] "Good morning, Josiah."',
    '[Thursday 09:01 Josiah Thorne] "Morning. I want milk today, remember that."',
    '[Thursday 09:02 Constance Scott] (paid Josiah Thorne 1 coin for milk)',
    '[Thursday 09:03 Josiah Thorne] "Ezekiel owes me, I said six coins to square us up."',
    '[Thursday 09:04 Ezekiel Crane] "Josiah, you are wrong about that."',
    '[Thursday 09:05 Constance Scott] (earned 4 coins working for Ezekiel Crane)',
    '[Thursday 09:06 Ezekiel Crane] (delivered meat to John Ellis for 4 coins)',
    '[Thursday 09:07 John Ellis] "Thank you, Constance."',
    '[Thursday 09:08 John Ellis] "Anne, the bread is late."',
    '[Thursday 09:09 Anne Hale] "Sorry John, I was worried about the oven."',
    'and it kept smoking',
    '[18:26 Constance Scott] (gave flour to Anne Hale)',
    '[Fri 10:00 Somebody] (paid Anne Hale 2 coins)',
].join('\n');

const CHAT_DAY = [
    'Session header line',
    '[10:00 home] I want this done by friday.',
    '[10:01 anne] Actually I prefer the other plan.',
    'continuation of anne',
    '[10:02 bob] ok',
    '- **From:** carol',
    'carol body line',
    'dave: hello there',
    '[2026-11-05 10:03] erin: because it matters',
    'frank:',
    '[x]  grace:nospace',
    '[[{"sender":"heidi","content":"I love it"},{"sender":"home","content":"same"},{"sender":5},{"sender":"ivan"}]',
    '  [{"sender":"judy","content":"remember this"}, {"sender":"Mallory Q","content":7}]',
    '[{"sender": broken',
    '---',
    '## heading',
    '[10:04 kim]x] trailing',
    '[10:05  ]',
    '[10:06 ]] odd',
    '[Monday 10:07   ]',
    '[Monday 10:08 ]',
    '[Sunday 1:00 Not Sim]',
    '[Holiday 10:09 Holly Day] hi',
    '[day 10:10 Short] hi',
    '- **From:**    zed  extra',
    '-**From:** nospace',
].join('\n');

out.prefilter = [
    '',
    'nothing here\nat all',
    SIM_DAY,
    CHAT_DAY,
    Array.from({ length: 30 }, (_, i) => (i === 12 ? 'I really hate this' : i === 25 ? 'no, stop' : 'line ' + i)).join('\n'),
    'a\nb\nREMEMBER the milk\nc',
    'Don\'t forget\nNo\tway\nno, way\nnone\nby   monday\ndue date\nfrustrated\nstressful\nmisses\nmiss',
    'x\n[{"sender":"a","content":"b"}]\ny\nI love you\nz',
    '[a] (paid)\r',
    '[a] (paid) \t',
    '[a] (paid) tail)',
    '[a] (pa\rid)',
    '[] (paid)',
    '[a](paid)',
    '[a] (paid) x',
    '[a] ((nested) paid)  ',
    'ſtop doing\nKeep in mınd\nthe reason',
].map((content) => ({ content, filtered: dream.prefilterLog(content) }));

function speakersOf(content, agent) {
    return [...dream.extractSpeakers(content, agent)].map(([slug, e]) => [slug, e.display, e.lines]);
}

const SPEAKER_INPUTS = [
    [CHAT_DAY, 'home'],
    [CHAT_DAY, 'zbbs-home'],
    [CHAT_DAY, ''],
    [SIM_DAY, 'Constance Scott'],
    [SIM_DAY, 'zbbs-josiah-thorne'],
    [SIM_DAY, 'salem-vendor'],
    ['[Thursday 09:00 Zoë Marsh] "hi"\n[Thursday 09:01 Zoe Marsh] "hi"\n[Thursday 09:02 ../etc] "x"', 'nobody'],
    ['[{"sender":"  Anne Hale ","content":"x"}]\nafter line', 'me'],
    ['[{"sender":"Anne\\nHale","content":"multi"}]', 'me'],
];
out.speakers = SPEAKER_INPUTS.map(([content, agent]) => ({ content, agent, speakers: speakersOf(content, agent) }));

out.sections = [
    [SIM_DAY, 'Constance Scott'],
    [SIM_DAY, 'zbbs-josiah-thorne'],
    [SIM_DAY, ''],
    [CHAT_DAY, 'home'],
    ['[Thursday 09:00 John Ellis] "John here"\n[Thursday 09:01 John Smith] "John there, Constance"\n[Thursday 09:02 Constance Scott] (paid John Ellis 1 coin)\n[Thursday 09:03 Zoë Marsh] "Zoëy said, Zoë Marsh came"', 'Constance Scott'],
    ['[{"sender":" Anne","content":"x"}]\n[Thursday 09:00 Bob Ray] " anne"', 'Self'],
].map(([content, self]) => {
    const filtered = dream.prefilterLog(content) || content;
    const sections = dream.buildPersonExcerptSections(filtered, self, dream.extractSpeakers(filtered, self));
    return { filtered, self, sections: [...sections].map(([slug, e]) => [slug, e.display, e.ledger, e.said]) };
});

out.personMessages = [
    { selfLabel: 'Constance Scott', display: 'Josiah Thorne', today: '2026-11-05', existingFile: '- old bullet', ledger: ['[x] (paid)', '[y] (got)'], said: 'line a\nline b' },
    { selfLabel: 'home', display: 'anne', today: '2026-11-05', existingFile: '', ledger: [], said: '   ' },
    { selfLabel: 'home', display: 'anne', today: '2026-11-05', existingFile: undefined, ledger: undefined, said: undefined },
    { selfLabel: 'home', display: 'anne', today: '2026-11-05', existingFile: 'f', ledger: 'not an array', said: '' },
].map((m) => ({ in: m, message: dream.buildPersonUserMessage(m) }));

out.soulMessages = [
    { agentName: 'home', startupInstructions: 'Be kind.', existingSoul: 'old soul', needsRebuild: false, backloadDreams: null, chunkDate: '2026-11-05', dreamContent: 'dream text' },
    { agentName: 'home', startupInstructions: '', existingSoul: 'stub', needsRebuild: true, backloadDreams: '### dreams/a\n\nA', chunkDate: '2026-11-05', dreamContent: 'dream text' },
    { agentName: 'home', startupInstructions: null, existingSoul: '', needsRebuild: true, backloadDreams: '', chunkDate: '2026-11-05', dreamContent: 'dream text' },
].map((m) => ({ in: m, message: dream.buildSoulUserMessage(m) }));

out.notesLogs = [
    [{ slug: 'journal/a', content: 'A', updated_at: '2026-05-18 14:22:00.123456+00' }],
    [{ slug: 'core', content: 'A', updated_at: '2026-05-16 08:00:00+00' }, { slug: 'thea', content: '', updated_at: '2026-05-16 23:59:59.999999+00' }],
].map((rows) => ({ rows, log: dream.buildNotesLog(rows.map((r) => ({ ...r, updated_at: parseTimestamptz(r.updated_at) }))) }));

out.chunks = [
    ['2026-11-04T20:00:00.123Z', '2026-11-06T15:30:00.000Z'],
    ['2026-11-06T00:00:00.000Z', '2026-11-06T00:00:00.000Z'],
    ['2026-11-06T10:00:00.000Z', '2026-11-06T09:00:00.000Z'],
    ['2026-11-05T23:59:59.999Z', '2026-11-06T00:00:00.001Z'],
    ['2026-11-06T00:00:00.000Z', '2026-11-08T00:00:00.000Z'],
].map(([since, now]) => ({ since, now, chunks: t.computeDailyChunks(new RealDate(since), new RealDate(now)).map((c) => [c.from.toISOString(), c.to.toISOString()]) }));

out.slugify = ['Morning Walk', '  --Hello, World!--  ', 'Ünïcödé Tïtle', 'İstanbul', 'a'.repeat(80), 'x'.repeat(59) + ' y', '', '!!!']
    .map((s) => [s, t.slugify(s)]);

out.titles = [
    '# Morning Walk\nbody', '#\n\nSpaced Title', '#   \n', '#  ', '#  x', 'Title:   Lower Case\nbody', 'TITLE: upper', 'title:',
    'title:\n\nnext', 'no title here', '  # not at start', 'x\n# Second Line Title  ', 'x\r# CR Title', '#NoSpace', '# A\n# B',
    'Title: first\n# Heading wins', '#  \nNext Line', 'title: \t\nNext', '#\t   Mixed','# Ünïcödé Tïtle!!', '# nbsp title', 'ｔitle: wide', 'tıtle: dotless',
].map((s) => [s, titleOf(s)]);

out.ledgerSpeakers = ['[Friday 18:26 Constance Scott] (x)', '[18:26 home] (x)', '[FRIDAY 9:05 Anne] (x)', '[Holiday  12:00   Bob ] x', '[123:45 Bob] (x)',
    '[day 12:00 X] (x)', '[] (x)', 'no header', '[Friday] (x)', '[2026-11-05 Bob] (x)', '[Fridayday 1:2 Q] (x)']
    .map((s) => [s, t.ledgerLineSpeaker(s), t.LEDGER_LINE.test(s)]);

out.jsonHeads = ['[{"sender":', ' [ { "sender" : x', '[{"sender"', '[{ "sender":', ' [ {"sender":', '[{"Sender":', '{"sender":']
    .map((s) => [s, t.JSON_ARRAY_HEAD.test(s)]);

out.names = [
    ['Anne', 'Anne went'], ['Anne', 'Annexed'], ['Anne', 'with anne.'], ['Zoë Marsh', 'zoë marsh!'], ['Zoë', 'Zoëy'], ['Zo', 'Zoë'],
    ['K', 'K'], ['ss', 'ß'], ['ß', 'ẞ'], ['A.B', 'AxB'], ['A.B', 'a.b'], ['', 'x'], ['', ''], ['', ' '], ['x', '_x'], ['x', 'x1'],
    ['Anne', 'ANNE'], ['İ', 'i'], ['ı', 'I'], ['Σ', 'ς'], ['😀', 'a😀b'], ['😀', 'a 😀 b'], ['x', '١x'], ['Δ', 'δ'],
].map(([frag, text]) => [frag, text, t.nameMatcher(frag).test(text)]);

out.preambles = ['', 'Clean soul text', '   Let me think about this', 'x'.repeat(195) + 'step 1: go', 'x'.repeat(200) + 'step 1: go',
    'I am <THINKING>', 'Analysis: fine', '## Analysis\nx', 'İ let me analyze'].map((s) => [s, t.REASONING_PREAMBLE_MARKERS && dream.detectReasoningPreamble(s)]);

out.rebuild = [['', 800], ['   ', 800], ['stub', 800], ['x'.repeat(800), 800], ['  ' + 'x'.repeat(799) + '  ', 800], ['stub', 0], ['stub', -1], ['😀'.repeat(400), 800]]
    .map(([s, n]) => [s, n, dream.soulNeedsRebuild(s, n)]);

function attempt(fn) {
    try { return { value: fn() }; } catch (e) { return { error: e.message }; }
}

out.prefixes = [undefined, null, '', 'constance-scott/', 'constance-scott', 'constance-scott//', ' constance-scott/', 'Constance-scott/',
    'a_b/', 'a%/', '../x/', 'a/b/', '-a/', 'a--b/', 'a'.repeat(99) + '/', 'a'.repeat(100) + '/', 5, {}, [], true]
    .map((v) => ({ in: v === undefined ? { undefined: true } : v, resolved: attempt(() => dream.resolveScopePrefix(v)), roster: dream.validateRosterPrefix(v) }));

out.peoplePaths = [['', 'anne'], [undefined, 'anne'], ['constance-scott/', 'josiah-thorne'], ['constance-scott/', '../x'], ['bad/', 'anne'],
    ['', 'Anne'], ['', ''], [5, 'anne'], ['', 'anne hale']]
    .map(([p, s]) => ({ prefix: p === undefined ? { undefined: true } : p, slug: s, path: attempt(() => dream.peopleNotePath(p, s)), valid: t.validatePersonSlug(s) }));

out.slugs = ['Anne Hale', '  Zoë  Marsh ', '../secrets', 'foo/bar', 'İstanbul', 'Kelvin', 'a--b', '-a-', '---', '', 'Ⅻ', 'ﬁne', '½ cup', 'José María', 'ǅemal', 'x​y']
    .map((s) => [s, personContextSlug(s)]);

out.slugPrefixes = ['constance-scott/', ' constance-scott ', 'a///', 'a', 'A/', 'a b/', '', '   ', '/'.repeat(5), 'a'.repeat(100) + '/', 'a'.repeat(1025), ' '.repeat(1020) + 'abc/', 'é/']
    .map((s) => [s, distiller.normalizeSlugPrefix(s), distiller.collapseTrailingSlashes(s)]);

out.displays = ['zbbs-john-ellis', 'Constance Scott', 'home', 'salem-vendor', 'zbbs--a--b-', '', 'ßtraße-ǆx', 'ŉame-ﬁre', '😀x-y', 'zbbs-', 'éa-ꞵb']
    .map((s) => [s, distiller.slugToDisplay(s)]);

out.delays = [['2000', 1000], ['0', 1000], ['', 1000], ['abc', 1000], ['12.9', 1000], ['-5', 1000], ['99999999999', 1000], ['0x10', 1000], ['  7x', 1000], [null, 1000]]
    .map(([v, fb]) => {
        sc = { config: v === null ? {} : { k: v } };
        return { value: v, fallback: fb, result: attempt(() => dream.dreamDelayMs('k', fb)) };
    });

// ---------------------------------------------------------------- passes

const CLOCK = '2026-11-06T15:30:00.000Z';
const BASE_CONFIG = {
    dream_processing_enabled: 'true',
    dream_interagent_delay: '2000',
    dream_interchunk_delay: '1000',
    dream_soul_min_chars: '800',
    dream_backload_count: '7',
    dream_cron_schedule: '0 4 * * *',
};
const TAGS = ['dream-companion', 'dream-technical', 'dream-sim', 'dream-companion-soul', 'dream-technical-soul', 'dream-sim-soul',
    'dream-companion-people', 'dream-sim-people', 'dream-sim-learnings'];
const AGENT_COLS = ['id', 'name', 'created_by', 'provider', 'model', 'api_key'];

// The nine expertise lookups. rows overrides a tag's rows; a tag left out
// has one trusted, configured agent named <tag>-va.
function lookups(rows = {}) {
    return TAGS.map((tag) => ({
        match: 'ac.expertise @>', columns: AGENT_COLS,
        rows: tag in rows ? rows[tag] : [[900, tag + '-va', 1, 'openrouter', 'm', 'k']],
    }));
}
const lockStep = (ok = true) => ({ match: 'pg_try_advisory_lock', columns: ['ok'], rows: [[ok]] });
const unlockStep = (ok = true) => ({ match: 'pg_advisory_unlock', columns: ['ok'], rows: [[ok]] });
const AGENT_ROW_COLS = ['name', 'actor_id', 'dream_mode', 'dream_source', 'last_dream_at', 'startup_instructions'];
const agentsStep = (rows) => ({ match: 'WHERE agc.dream_mode IN', columns: AGENT_ROW_COLS, rows });
const probe = (hasConversations, hasDreams, minNote) => ({ match: 'AS has_conversations', columns: ['has_conversations', 'has_dreams', 'min_note_updated'], rows: [[hasConversations, hasDreams, minNote]] });
const convLogs = (contents, extra = {}) => ({ match: 'slug LIKE $4', columns: ['slug', 'content', 'created_at'], rows: contents.map((c, i) => ['conversations/c' + i, c, '2026-11-05 10:0' + i + ':00+00']), ...extra });
const notesLogs = (rows) => ({ match: '($4::text IS NULL', columns: ['slug', 'content', 'updated_at'], rows });
const cursor = (extra = {}) => ({ match: 'UPDATE agent_configuration SET last_dream_at', ...extra });
const sharedCursor = (extra = {}) => ({ match: 'UPDATE sim_shared_actor SET last_dream_at', ...extra });
const roster = (rows, extra = {}) => ({ match: 'FROM sim_shared_actor', columns: ['slug_prefix', 'display_name', 'last_dream_at'], rows, ...extra });
const minCreated = (v) => ({ match: 'AS min_created', columns: ['min_created'], rows: [[v]] });
const minUpdated = (v) => ({ match: 'AS min_updated', columns: ['min_updated'], rows: [[v]] });

const HEALTHY_SOUL = 'I am steady. '.repeat(70);
const CHAT_LOG = '[10:00 subject] I want the report by friday, remember that.\n[10:01 anne] Actually I prefer Monday.\n[10:02 bob] Because the build is wrong.\n[10:03 subject] ok';

const passes = [];
function pass(name, s) {
    passes.push({ ...s, name, clock: s.clock || CLOCK, config: { ...BASE_CONFIG, ...(s.config || {}) }, drop: s.drop || [] });
}

pass('disabled', { config: { dream_processing_enabled: 'false' }, script: [] });
pass('enabled-key-missing', { drop: ['dream_processing_enabled'], script: [] });
pass('lock-held', { script: [lockStep(false)] });
pass('lock-null', { script: [{ match: 'pg_try_advisory_lock', columns: ['ok'], rows: [[null]] }] });
pass('connect-fails', { connectError: 'simulated pool.connect failure', script: [] });
pass('lock-query-fails', { script: [{ match: 'pg_try_advisory_lock', error: 'simulated pg_try_advisory_lock failure' }] });
pass('no-dream-agents', {
    script: [lockStep(), ...lookups(Object.fromEntries(TAGS.map((t) => [t, []]))), unlockStep()],
});
pass('dream-agent-checks', {
    trusted: [1, 7],
    script: [lockStep(), ...lookups({
        'dream-companion': [[1, 'untrusted-va', 5, 'p', 'm', 'k'], [2, 'nokey-va', 7, 'p', 'm', '']],
        'dream-technical': [[3, 'null-creator', null, 'p', 'm', 'k'], [4, 'tech-va', 1, 'p', 'm', 'k']],
        'dream-sim': [[5, 'noprovider-va', 1, null, 'm', 'k']],
        'dream-sim-soul': [],
    }), agentsStep([]), unlockStep()],
});
pass('trust-error', { trustError: 'simulated permission lookup failure', script: [lockStep(), lookups()[0], unlockStep()] });
pass('agents-query-fails', { script: [lockStep(), ...lookups(), { match: 'WHERE agc.dream_mode IN', error: 'simulated agent-list query failure' }, unlockStep()] });

// A companion agent over three days: a full first day, a day without
// signals, and a failing third.
pass('companion-three-days', {
    notesIn: {
        'context/soul': HEALTHY_SOUL,
        // Trailing whitespace: the unchanged reply is not written.
        'context/people/anne': '- likes Mondays\n\n',
    },
    answers: [
        { text: '# Report Deadlines\n\nThe user wants the report.' },
        { text: '  New soul text, steady and kind.  ' },
        { text: '- likes Mondays' },
        { text: '- thinks the build is wrong' },
        { error: 'simulated provider outage' },
    ],
    script: [lockStep(), ...lookups(),
        agentsStep([['subject', 11, 'companion', 'conversation', '2026-11-04 20:00:00.123456+00', 'Be helpful.']]),
        probe(true, true, '2026-01-01 00:00:00+00'),
        convLogs([CHAT_LOG, 'a quiet second log']),
        cursor(),
        convLogs(['nothing to see', 'still nothing']),
        cursor(),
        convLogs(['I want more']),
        unlockStep()],
});

pass('companion-no-logs-and-delays', {
    config: { dream_interagent_delay: '99999999999', dream_interchunk_delay: '0' },
    script: [lockStep(), ...lookups(),
        agentsStep([
            ['one', 11, 'companion', 'conversation', '2026-11-06 12:00:00+00', null],
            ['two', 12, 'technical', 'conversation', '2026-11-06 13:00:00+00', null],
        ]),
        probe(true, false, null), convLogs([]), cursor(),
        probe(true, false, null), convLogs([]), cursor(),
        unlockStep()],
});

pass('delay-values', {
    config: { dream_interagent_delay: 'abc', dream_interchunk_delay: '-5' },
    script: [lockStep(), ...lookups(),
        agentsStep([['one', 11, 'technical', 'conversation', '2026-11-05 12:00:00+00', null]]),
        probe(true, false, null), convLogs([]), cursor(), convLogs([]), cursor(),
        unlockStep()],
});

pass('interchunk-key-missing', {
    drop: ['dream_interchunk_delay'],
    script: [lockStep(), ...lookups(),
        agentsStep([['one', 11, 'technical', 'conversation', '2026-11-05 12:00:00+00', null]]),
        probe(true, false, null), unlockStep()],
});

pass('interagent-key-missing', {
    drop: ['dream_interagent_delay'],
    script: [lockStep(), ...lookups(),
        agentsStep([['one', 11, 'technical', 'conversation', '2026-11-06 12:00:00+00', null], ['two', 12, 'technical', 'conversation', null, null]]),
        probe(true, false, null), convLogs([]), cursor(), unlockStep()],
});

pass('soul-rebuild-backload', {
    lists: { 'dreams/': ['dreams/2026-11-06-x', 'dreams/missing', 'dreams/2026-11-04-y'] },
    notesIn: { 'dreams/2026-11-06-x': 'Dream X', 'dreams/2026-11-04-y': 'Dream Y' },
    answers: [{ text: 'title: Work Log\nThe day was about work.' }, { text: 'Let me analyze the soul first.\nThen the soul.' }],
    script: [lockStep(), ...lookups(),
        agentsStep([['tech', 11, 'technical', 'conversation', '2026-11-06 10:00:00+00', '']]),
        probe(true, false, null), convLogs(['It is important to ship.']), cursor(), unlockStep()],
});

pass('soul-degraded-no-backload', {
    config: { dream_backload_count: '0' },
    notesIn: { 'context/soul': '  short stub  ' },
    answers: [{ text: 'no title' }, { text: 'cut off mid', truncated: true, finish: 'length' }],
    script: [lockStep(), ...lookups(),
        agentsStep([['tech', 11, 'technical', 'conversation', '2026-11-06 10:00:00+00', null]]),
        probe(true, false, null), convLogs(['I hate typos.']), cursor(), unlockStep()],
});

pass('soul-variants', {
    drop: ['dream_soul_min_chars', 'dream_backload_count'],
    notesIn: { 'context/soul': 'tiny' },
    answers: [
        { text: '# Day One' }, { text: '   ', usage: { input_tokens: 9, output_tokens: 42 } },
        { text: '# Day Two' }, { text: null, usage: null },
        { text: '# Day Three' }, { text: [1, 2] },
        { text: '# Day Four' }, { text: [1], truncated: true },
        { text: '# Day Five' }, { error: 'soul writer down' },
        { text: '# Day Six' }, { text: 7, truncated: true },
    ],
    script: [lockStep(), ...lookups(),
        agentsStep([['tech', 11, 'technical', 'conversation', '2026-11-01 00:00:00+00', null]]),
        probe(true, false, null),
        convLogs(['I love one.']), cursor(), convLogs(['I love two.']), cursor(), convLogs(['I love three.']), cursor(),
        convLogs(['I love four.']), cursor(), convLogs(['I love five.']), cursor(), convLogs(['I love six.']), cursor(),
        unlockStep()],
});

pass('sim-day', {
    notesIn: {
        'context/soul': HEALTHY_SOUL,
        'context/people/josiah-thorne': '- sells milk',
        'learnings/2026-11-05-sim-day': '- earlier learning',
    },
    answers: [
        { text: '# Market Day\nConstance bought milk.' },
        { text: 'Soul of Constance.' },
        { text: '- sells milk\n- claims a debt' },
        { text: '- hired me' },
        { text: '- bakes late', truncated: true, finish: 'length' },
        { error: 'people writer down' },
        { text: '- learned the price of milk' },
    ],
    failNth: { save: [null, null, null, null] },
    script: [lockStep(), ...lookups(),
        agentsStep([['zbbs-constance-scott', 21, 'sim', 'conversation', '2026-11-05 00:00:00+00', 'A seamstress.']]),
        convLogs([SIM_DAY]), cursor(), convLogs([]), cursor(), unlockStep()],
});

pass('sim-learnings-variants', {
    answers: [
        { text: '# A' }, { text: 'NONE' },
        { text: '# B' }, { text: 'cut', truncated: true },
        { text: '# C' }, { error: 'learnings down' },
        { text: '# D' }, { text: { a: 1 } },
        { text: '# E' }, { text: '  none  ' },
    ],
    script: [lockStep(), ...lookups({ 'dream-sim-soul': [], 'dream-sim-people': [] }),
        agentsStep([['zbbs-x', 21, 'sim', 'conversation', '2026-11-01 12:00:00+00', null]]),
        convLogs(['[Monday 09:00 X] "I want A"']), cursor(),
        convLogs(['[Monday 09:00 X] "I want B"']), cursor(),
        convLogs(['[Monday 09:00 X] "I want C"']), cursor(),
        convLogs(['[Monday 09:00 X] "I want D"']), cursor(),
        convLogs(['[Monday 09:00 X] "I want E"']), cursor(),
        convLogs([]), cursor(),
        unlockStep()],
});

pass('dream-text-shapes', {
    failNth: { save: ['Note already exists: dreams/x'] },
    answers: [{ text: null }, { text: 5 }, { text: 'title: Dupe' }, { text: '' }],
    script: [lockStep(), ...lookups({ 'dream-technical-soul': [] }),
        agentsStep([
            ['a', 11, 'technical', 'conversation', '2026-11-06 10:00:00+00', null],
            ['b', 12, 'technical', 'conversation', '2026-11-06 10:00:00+00', null],
            ['c', 13, 'technical', 'conversation', '2026-11-06 10:00:00+00', null],
            ['d', 14, 'technical', 'conversation', '2026-11-06 10:00:00+00', null],
        ]),
        probe(true, false, null), convLogs(['remember a']),
        probe(true, false, null), convLogs(['remember b']),
        probe(true, false, null), convLogs(['remember c']),
        probe(true, false, null), convLogs(['remember d']), cursor(),
        unlockStep()],
});

pass('cursor-and-query-failures', {
    answers: [{ text: '# T' }],
    script: [lockStep(), ...lookups({ 'dream-technical-soul': [] }),
        agentsStep([
            ['a', 11, 'technical', 'conversation', '2026-11-06 10:00:00+00', null],
            ['b', 12, 'technical', 'conversation', '2026-11-06 10:00:00+00', null],
            ['c', 13, 'technical', 'conversation', '2026-11-06 10:00:00+00', null],
        ]),
        probe(true, false, null), convLogs(['remember a']), cursor({ error: 'simulated cursor failure' }),
        probe(true, false, null), { match: 'slug LIKE $4', error: 'simulated conversation-log query failure' },
        { match: 'AS has_conversations', error: 'simulated probe failure' },
        unlockStep()],
});

pass('notes-modes', {
    answers: [{ text: '# Notes Day' }, { text: 'soul from notes' }, { text: '# Fallback Day' }, { text: 'soul two' }, { text: '# Kept Cursor' }, { text: 'soul three' }],
    script: [lockStep(), ...lookups(),
        agentsStep([
            ['explicit', 11, 'companion', 'notes', null, null],
            ['empty', 12, 'technical', 'notes', null, null],
            ['fallback', 13, 'companion', 'conversation', '2026-11-06 12:00:00+00', null],
            ['dreamed', 14, 'technical', 'conversation', '2026-11-06 12:00:00+00', null],
            ['busy', 15, 'companion', 'conversation', '2026-11-06 12:00:00+00', null],
            ['simmer', 16, 'sim', 'conversation', '2026-11-06 12:00:00+00', null],
        ]),
        minUpdated('2026-11-06 08:15:00.500999+00'),
        notesLogs([['journal/a', 'Journal A', '2026-11-06 08:15:00.500999+00'], ['ideas', 'speaker: not a speaker', '2026-11-06 09:00:00+00']]),
        cursor(),
        minUpdated(null),
        probe(false, false, '2026-11-06 07:00:00+00'),
        notesLogs([['note-1', 'Fallback note', '2026-11-06 07:00:00+00']]),
        cursor(),
        probe(false, true, '2026-11-06 07:00:00+00'),
        notesLogs([['note-2', 'Kept note', '2026-11-06 13:00:00+00']]),
        cursor(),
        probe(true, false, '2026-11-06 07:00:00+00'),
        convLogs([]), cursor(),
        convLogs([]), cursor(),
        unlockStep()],
});

pass('windows', {
    script: [lockStep(), ...lookups(),
        agentsStep([
            ['future', 11, 'technical', 'conversation', '2026-11-07 00:00:00+00', null],
            ['first', 12, 'technical', 'conversation', null, null],
            ['nomode', 13, 'technical', 'conversation', '2026-11-06 15:29:59.9995+00', null],
        ]),
        probe(true, false, null),
        probe(true, false, null), convLogs([]), cursor(), convLogs([]), cursor(),
        probe(true, false, null), convLogs([]), cursor(),
        unlockStep()],
});

pass('missing-mode-agents', {
    script: [lockStep(), ...lookups({ 'dream-technical': [], 'dream-sim': [] }),
        agentsStep([['t', 11, 'technical', 'conversation', null, null], ['s', 12, 'sim', 'conversation', null, null], ['ss', 13, 'sim-shared', 'conversation', null, null]]),
        unlockStep()],
});

const JOSIAH_DAY = [
    '[Thursday 09:00 Josiah Thorne] "Morning, Moses."',
    '[Thursday 09:01 Moses James] "I want two pails."',
    '[Thursday 09:02 Josiah Thorne] (sold 2 pails to Moses James for 2 coins)',
].join('\n');

pass('sim-shared-roster', {
    answers: [{ text: '# Pails' }, { text: '- buys pails' }, { text: '- pails learned' }],
    script: [lockStep(), ...lookups(),
        agentsStep([['salem-vendor', 42, 'sim-shared', 'conversation', null, null]]),
        roster([
            ['Bad/', 'Bad', null],
            ['blank-name/', '   ', null],
            ['constance-scott/', 'Constance Scott', null],
            ['josiah-thorne/', ' Josiah Thorne ', '2026-11-06 06:00:00+00'],
            ['late/', 'Late', '2026-11-07 06:00:00+00'],
            ['moses-james/', 'Moses James', null],
            ['zed/', 'Zed', '2026-11-06 06:00:00+00'],
        ]),
        minCreated(null),
        convLogs([JOSIAH_DAY]),
        sharedCursor(),
        minCreated('2026-11-06 01:00:00.000001+00'),
        { match: 'slug LIKE $4', error: 'simulated conversation-log query failure' },
        { match: 'slug LIKE $4', error: 'simulated zed failure' },
        unlockStep()],
});

pass('sim-shared-variants', {
    script: [lockStep(), ...lookups(),
        agentsStep([
            ['pool-empty', 41, 'sim-shared', 'conversation', null, null],
            ['pool-notes', 42, 'sim-shared', 'notes', null, null],
            ['pool-broken', 43, 'sim-shared', 'conversation', null, null],
            ['pool-cursor', 44, 'sim-shared', 'conversation', null, null],
        ]),
        roster([]),
        roster([], { error: 'simulated roster failure' }),
        roster([['a/', 'A', '2026-11-06 15:00:00+00']]),
        convLogs([]),
        sharedCursor({ error: 'simulated shared cursor failure' }),
        unlockStep()],
});

pass('sim-shared-no-sim-agent', {
    script: [lockStep(), ...lookups({ 'dream-sim': [] }), agentsStep([['pool', 41, 'sim-shared', 'conversation', null, null]]), unlockStep()],
});

pass('sim-shared-delay-key-missing', {
    drop: ['dream_interagent_delay'],
    script: [lockStep(), ...lookups(), agentsStep([['pool', 41, 'sim-shared', 'conversation', null, null]]), roster([['a/', 'A', null]]), unlockStep()],
});

pass('lock-lost-during-chunk', {
    answers: [{ text: '# Lost' }],
    script: [lockStep(), ...lookups({ 'dream-technical-soul': [] }),
        agentsStep([['a', 11, 'technical', 'conversation', '2026-11-06 10:00:00+00', null]]),
        probe(true, false, null), convLogs(['remember a'], { lose: 'Connection terminated unexpectedly' }),
        unlockStep(false)],
});

pass('lock-lost-at-cursor', {
    script: [lockStep(), ...lookups(),
        agentsStep([['a', 11, 'technical', 'conversation', '2026-11-06 10:00:00+00', null], ['b', 12, 'technical', 'conversation', null, null]]),
        probe(true, false, null), convLogs([]), cursor({ lose: 'terminating connection due to administrator command' }),
        { match: 'pg_advisory_unlock', error: 'simulated pg_advisory_unlock failure' }],
});

pass('lock-lost-in-roster', {
    script: [lockStep(), ...lookups(),
        agentsStep([['pool', 41, 'sim-shared', 'conversation', null, null]]),
        roster([['a/', 'A', '2026-11-06 15:00:00+00'], ['b/', 'B', '2026-11-06 15:00:00+00']], { lose: 'socket hang up' }),
        unlockStep()],
});

pass('lock-lost-after-last-agent', {
    config: { dream_interagent_delay: '0' },
    script: [lockStep(), ...lookups(),
        agentsStep([['a', 11, 'technical', 'conversation', '2026-11-06 10:00:00+00', null]]),
        probe(true, false, null, ), convLogs([]), cursor({ lose: 'late loss' }), unlockStep()],
});

pass('unlock-not-held', {
    script: [lockStep(), ...lookups(), agentsStep([]), unlockStep(false)],
});

pass('unlock-fails', {
    script: [lockStep(), ...lookups(), agentsStep([]), { match: 'pg_advisory_unlock', error: 'simulated pg_advisory_unlock failure' }],
});

pass('unlock-no-row', {
    script: [lockStep(), ...lookups(), agentsStep([]), { match: 'pg_advisory_unlock', columns: ['ok'], rows: [] }],
});

pass('people-edge-cases', {
    notesIn: { 'context/soul': HEALTHY_SOUL },
    answers: [{ text: '# People' }, { text: 'soul' }, { text: '- anne file' }, { text: '- quoted file' }, { text: '' }],
    failNth: { save: [null, null, 'save failed for anne'] },
    script: [lockStep(), ...lookups(),
        agentsStep([['subject', 11, 'companion', 'conversation', '2026-11-06 10:00:00+00', null]]),
        probe(true, false, null),
        convLogs(['[{"sender":"Anne\\r\\nHale","content":"I want x"},{"sender":"  Bo  Peep ","content":"y"}]\n[{"sender":"../etc","content":"z"}]']),
        cursor(), unlockStep()],
});

for (const s of passes) {
    sc = { ...s, script: s.script.map((x) => ({ ...x })), answers: [...(s.answers || [])], counts: {} };
    for (const k of s.drop) delete sc.config[k];
    events = [];
    clock = RealDate.parse(s.clock);
    scheduled = null;
    const cfgForStart = sc.config;
    sc.config = { ...cfgForStart, dream_cron_schedule: '0 4 * * *' };
    dream.startDreamScheduler();
    sc.config = cfgForStart;
    if (!scheduled) throw new Error(s.name + ': the scheduler did not start');
    events = [];
    // The scheduler's own callback, as the cron would run it.
    // eslint-disable-next-line no-await-in-loop
    s.run = scheduled;
}

async function runPasses() {
    const results = [];
    for (const s of passes) {
        sc = { ...s, script: s.script.map((x) => ({ ...x })), answers: [...(s.answers || [])], counts: {} };
        sc.config = { ...s.config };
        for (const k of s.drop) delete sc.config[k];
        events = [];
        clock = RealDate.parse(s.clock);
        await s.run();
        // Let the microtask-queued timers and fire-and-forget calls settle.
        for (let i = 0; i < 20; i++) await Promise.resolve();
        results.push({
            name: s.name, clock: s.clock, config: sc.config, connectError: s.connectError || '', trusted: s.trusted || [1],
            trustError: s.trustError || '', notesIn: s.notesIn || {}, lists: s.lists || {}, failNth: s.failNth || {},
            answers: s.answers || [], script: s.script, unused: sc.script.length, unusedAnswers: sc.answers.length, events,
        });
    }
    return results;
}

async function runStarts() {
    const schedules = ['', '0 4 * * *', '@daily', 'not a cron', '0 0 31 2 *', '61 * * * *', null];
    const results = [];
    for (const schedule of schedules) {
        sc = { config: schedule === null ? {} : { dream_cron_schedule: schedule } };
        events = [];
        scheduled = null;
        let threw = '';
        try {
            dream.startDreamScheduler();
        } catch (e) {
            threw = e.message;
        }
        // node-cron arms (and the stub then stops) a real timer; its
        // setTimeout is not the job's.
        results.push({ schedule, started: !!scheduled, threw, events: events.filter((e) => e.kind !== 'sleep') });
    }
    return results;
}

(async () => {
    out.passes = await runPasses();
    out.starts = await runStarts();
    for (const p of out.passes) {
        if (p.unused || p.unusedAnswers) console.error(`${p.name}: ${p.unused} statements and ${p.unusedAnswers} answers unused`);
        const err = p.events.find((e) => e.kind === 'log' && /unscripted|does not match|no answer scripted/.test(e.details));
        if (err) console.error(`${p.name}: script mismatch: ${err.details}`);
    }
    fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify(out, null, 1) + '\n');
})();
