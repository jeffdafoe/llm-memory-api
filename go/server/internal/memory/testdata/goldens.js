// Writes goldens.json beside this file: what the Node server's search
// pipeline produces for a set of inputs, so the Go port's unit tests can
// compare against Node itself instead of hand-written expected values.
//
// Run from the repository root, after `npm ci` in node/api:
//
//     node go/server/internal/memory/testdata/goldens.js
//
// It covers the pure parts (query cleanup, LIKE escaping, chunking) and the
// SQL that searchMemory sends for each config branch. The database and the
// embeddings call are replaced with stubs, so nothing leaves the process.

const fs = require('fs');
const path = require('path');

const api = path.resolve(__dirname, '../../../../../node/api/src');

// Swap the database pool and the embeddings client for stubs before
// memory.js loads them. Node caches modules by resolved file name, so a
// cache entry put in first is what require() returns to memory.js.
const queries = [];
function stub(file, exports) {
    const resolved = require.resolve(path.join(api, file));
    require.cache[resolved] = { id: resolved, filename: resolved, loaded: true, exports };
}
stub('db.js', {
    query: async (sql, params) => {
        queries.push({ sql, params });
        return { rows: [], rowCount: 0 };
    },
});
const EMBEDDING = [0.25, -0.5, 1e-7, 3];
stub('services/embeddings.js', { embed: async () => [EMBEDDING] });

const config = require(path.join(api, 'services/config.js'));
const memory = require(path.join(api, 'services/memory.js'));
const chunker = require(path.join(api, 'services/chunker.js'));

// The values seed.sql ships (migrations/seed.sql).
const SEED_CONFIG = {
    search_pool_multiplier: '3',
    search_filename_boost: '0.15',
    search_bm25_boost: '0.1',
    search_min_similarity: '0',
    search_decay_halflife_task: '60',
    search_decay_halflife_learning: '90',
    search_decay_halflife_note: '180',
    search_decay_halflife_reference: '0',
    search_decay_halflife_instruction: '0',
    search_decay_halflife_conversation: '30',
    search_decay_halflife_dream: '30',
    search_decay_halflife_semantic: '0',
    search_decay_halflife_episodic: '90',
    search_decay_halflife_procedural: '0',
    search_decay_halflife_reflective: '180',
    search_access_boost_max: '0.05',
    search_access_boost_window_days: '14',
    search_conversation_weight: '0.7',
    search_dream_weight: '1.0',
};

function setConfig(overrides) {
    for (const [k, v] of Object.entries({ ...SEED_CONFIG, ...overrides })) {
        config.set(k, v);
    }
}

// Each search case: the arguments searchMemory takes, plus config overrides.
const searchCases = [
    { name: 'one namespace', query: 'how does the auth middleware work', namespace: 'agent-a', limit: 5 },
    { name: 'wildcard, all readable', query: 'auth middleware', namespace: '*', limit: 5, readable: null },
    { name: 'no namespace, all readable', query: 'auth middleware', limit: 5, readable: null },
    { name: 'wildcard with grants and actor', query: 'auth middleware', namespace: '*', limit: 5, readable: ['shared', 'agent-b'], actorId: 7 },
    { name: 'wildcard with grants, no actor', query: 'auth middleware', limit: 5, readable: ['shared'] },
    { name: 'wildcard with no grants', query: 'auth middleware', limit: 5, readable: [], actorId: 7 },
    { name: 'slug prefix', query: 'recall', namespace: 'shared', limit: 3, slugPrefix: 'Anne_Walker/100%/' },
    { name: 'slug prefix empty is no filter', query: 'recall', namespace: 'shared', limit: 3, slugPrefix: '' },
    { name: 'floor on', query: 'auth middleware', namespace: 'agent-a', limit: 5, config: { search_min_similarity: '0.3' } },
    { name: 'access boost off by max', query: 'auth', namespace: 'agent-a', limit: 5, config: { search_access_boost_max: '0' } },
    { name: 'access boost off by window', query: 'auth', namespace: 'agent-a', limit: 5, config: { search_access_boost_window_days: '0' } },
    { name: 'no decay anywhere', query: 'auth', namespace: 'agent-a', limit: 5, config: Object.fromEntries(Object.keys(SEED_CONFIG).filter(k => k.startsWith('search_decay_')).map(k => [k, '0'])) },
    { name: 'short words only', query: 'an ox', namespace: 'agent-a', limit: 5 },
    { name: 'filler only falls back', query: 'the', namespace: 'agent-a', limit: 5 },
    { name: 'limit float', query: 'auth', namespace: 'agent-a', limit: 0.15 },
    { name: 'limit large', query: 'auth', namespace: 'agent-a', limit: 50 },
    { name: 'limit pool cap', query: 'auth', namespace: 'agent-a', limit: 400 },
    { name: 'limit string', query: 'auth', namespace: 'agent-a', limit: '7' },
    { name: 'limit missing', query: 'auth', namespace: 'agent-a' },
    { name: 'limit null', query: 'auth', namespace: 'agent-a', limit: null },
    { name: 'limit negative', query: 'auth', namespace: 'agent-a', limit: -3 },
    { name: 'limit array of one', query: 'auth', namespace: 'agent-a', limit: [7] },
    { name: 'limit true', query: 'auth', namespace: 'agent-a', limit: true },
    { name: 'limit object', query: 'auth', namespace: 'agent-a', limit: {} },
    { name: 'pool multiplier zero', query: 'auth', namespace: 'agent-a', limit: 5, config: { search_pool_multiplier: '0' } },
    { name: 'pool multiplier capped', query: 'auth', namespace: 'agent-a', limit: 5, config: { search_pool_multiplier: '20' } },
    { name: 'pool multiplier rounds', query: 'auth', namespace: 'agent-a', limit: 3, config: { search_pool_multiplier: '2.5' } },
    { name: 'pool multiplier garbage', query: 'auth', namespace: 'agent-a', limit: 5, config: { search_pool_multiplier: 'abc' } },
    { name: 'weights and boosts tuned', query: 'auth middleware', namespace: 'agent-a', limit: 5, config: { search_conversation_weight: '0.30000000000000004', search_dream_weight: '0', search_filename_boost: '1e-7', search_bm25_boost: '0x10' } },
    { name: 'negative config falls back', query: 'auth', namespace: 'agent-a', limit: 5, config: { search_filename_boost: '-1', search_decay_halflife_episodic: '-5', search_access_boost_max: 'Infinity' } },
    { name: 'mixed case and unicode words', query: 'Tell me about the ΟΔΟΣ İstanbul Ünïcode', namespace: 'agent-a', limit: 5 },
];

const preprocessInputs = [
    'how does the auth middleware work',
    'can you tell me about how the auth middleware works',
    'Please explain regarding the dream lock',
    'CAN YOU TELL ME about it',
    'what is',
    'the a an',
    '  leading and trailing  ',
    'tabs\tand\nnewlines   collapse',
    'x',
    '',
    'ab',
    'thanks THANK you',
    'İ İstanbul',
    'ΟΔΟΣ',
    'nbsp split em',
    '﻿bom start',
    'nel\u0085not js space',
    'how do i deploy',
    'how does it deploy',
    'how do we deploy',
    'tell me show me explain the thing',
    'a'.repeat(memory.MAX_EMBED_QUERY_CHARS + 50),
    'a'.repeat(memory.MAX_EMBED_QUERY_CHARS - 1) + '😀😀',
    'b'.repeat(memory.MAX_EMBED_QUERY_CHARS - 2) + '😀😀',
    'the '.repeat(5000),
];

const likeInputs = ['anne-walker/memory/', 'anne_walker/', '100%/', '\\_', 'a%b_c\\d', ''];

const headingInputs = [
    ['# A Day of Trade and Hospitality at the Inn', '', '## Notable scenes', 'Hannah had a series of conversations with the villagers.', '', '## Decisions', 'Hannah decided to trade flour for nails.'].join('\n'),
    ['## Intro', 'First paragraph.', '', '## Body', 'Second paragraph.'].join('\n'),
    ['# Title', 'Prose right under the title.'].join('\n'),
    'Just a paragraph of prose with no headings at all.',
    ['## Real section', 'Has content.', '', '## Empty trailer'].join('\n'),
    ['# Title Only', '## Section A', '## Section B'].join('\n'),
    '',
    '   \n\n  ',
    'preamble before any heading\n# H1\nbody\n#### not a heading\n##no space\n###\ttab heading\nmore',
    '\r\n# CRLF heading\r\nbody line\r\n',
    '  # indented is not a heading\nprose',
    '# nbsp heading\nprose under nbsp',
    '﻿# bom heading\nprose',
    '# only\n \n',
];

const conversationInputs = [
    { content: 'Session: x\n---\n[10:00 work] one\n[10:01 jeff] two\n[10:02 work] three\n[10:03 jeff] four\n[10:04 work] five\n[10:05 jeff] six\n[10:06 work] seven', window: 5, overlap: 2, maxChars: 0 },
    { content: 'Session: x\n---\n[10:00 work] one\ncontinued line\n[10:01 jeff] two\n[10:02 work] three', window: 2, overlap: 1, maxChars: 0 },
    { content: '[10:00 work] ' + 'x'.repeat(30) + '\n[10:01 jeff] ' + 'y'.repeat(30) + '\n[10:02 work] ' + 'z'.repeat(30) + '\n[10:03 jeff] short', window: 5, overlap: 2, maxChars: 50 },
    { content: 'no messages here, just text', window: 5, overlap: 2, maxChars: 0 },
    { content: '   ', window: 5, overlap: 2, maxChars: 0 },
    { content: '[10:00 a] one\n[10:01 b] two\n[10:02 c] three', window: 2, overlap: 5, maxChars: 0 },
    { content: '[10:00 a] one\n[10:01 b] two\n[10:02 c] three', window: 0, overlap: 0, maxChars: 0 },
    { content: '[10:00 a] one\n[10:01 b] two\n[10:02 c] three\n[10:03 d] four', window: -2, overlap: 2, maxChars: 0 },
    { content: '[10:00 a] 😀😀😀😀\n[10:01 b] 😀😀😀😀\n[10:02 c] three', window: 5, overlap: 0, maxChars: 20 },
    { content: 'header\n[1:00 bad time] not a message\n[10:00 a] one', window: 5, overlap: 2, maxChars: 0 },
];

async function main() {
    const search = [];
    for (const c of searchCases) {
        setConfig(c.config || {});
        queries.length = 0;
        await memory.searchMemory(c.query, c.namespace, c.limit, c.readable, c.actorId, c.slugPrefix);
        if (queries.length !== 1) {
            throw new Error(`${c.name}: expected one query, got ${queries.length}`);
        }
        search.push({
            name: c.name,
            args: {
                query: c.query,
                namespace: c.namespace === undefined ? null : c.namespace,
                limit: c.limit === undefined ? null : c.limit,
                limit_missing: !('limit' in c),
                readable: c.readable === undefined ? null : c.readable,
                actor_id: c.actorId || 0,
                slug_prefix: c.slugPrefix === undefined ? null : c.slugPrefix,
            },
            config: { ...SEED_CONFIG, ...(c.config || {}) },
            sql: queries[0].sql,
            params: queries[0].params,
        });
    }

    const out = {
        embedding: EMBEDDING,
        max_embed_query_chars: memory.MAX_EMBED_QUERY_CHARS,
        preprocess: preprocessInputs.map(q => ({ in: q, out: memory.preprocessQuery(q) })),
        escape_like: likeInputs.map(v => ({ in: v, out: memory.escapeLikePattern(v) })),
        chunk_by_heading: headingInputs.map(c => ({ in: c, out: chunker.chunkByHeading(c) })),
        chunk_conversation: conversationInputs.map(c => ({ ...c, out: chunker.chunkConversation(c.content, c.window, c.overlap, c.maxChars) })),
        search,
    };
    fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify(out, null, 2) + '\n');
}

main().catch(err => {
    console.error(err);
    process.exit(1);
});
