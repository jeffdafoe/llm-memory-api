// Writes goldens.json beside this file: what the Node server's notes
// helpers return for a set of inputs, so the Go port's unit tests compare
// against Node itself. Run from the repository root, after `npm ci` in
// node/api:
//
//     node go/server/internal/notes/testdata/goldens.js
//
// The database is replaced with a stub; nothing leaves the process.

const fs = require('fs');
const path = require('path');

const api = path.resolve(__dirname, '../../../../../node/api/src');
const resolved = require.resolve(path.join(api, 'db.js'));
require.cache[resolved] = { id: resolved, filename: resolved, loaded: true, exports: { query: async () => ({ rows: [] }) } };

const docs = require(path.join(api, 'services/documents.js'));

function outcome(fn) {
    try {
        return { ok: fn() === undefined ? true : fn() };
    } catch (err) {
        return { error: err.message, status: err.statusCode };
    }
}

const titles = [
    'Hello World', '  Leading and trailing  ', 'Tabs\tand\nnewlines', 'Ünïcödé Title', 'İstanbul',
    'Already-slugged-title', '--dashes--everywhere--', 'Mixed   spaces nbsp em',
    'punctuation!@#$%^&*()_+=[]{}|;:\'",.<>/?', '!!!', '', 'a'.repeat(250), '123 numbers 456',
    'ΟΔΟΣ', '-', 'a - b', 'x﻿y',
];

const slugs = [
    'notes/x', '', 'a'.repeat(500), 'a'.repeat(501), 'notes/\u0001bad', 'notes\\x', '/leading', 'trailing/',
    'a//b', 'a/b/', 'ok/with space', 'tab\there', 'del\u007f', 'émoji/😀', 'x', '.', 'a/./b',
];

const kinds = [
    'instructions/x', 'notes/codebase/x', 'conversations/x', 'context/soul', 'dreams/x', 'tasks/done/x',
    'tasks/x', 'learnings/x', 'notes/x', 'other/x', 'Notes/Codebase/x', 'instructions',
];

const content = ['line 1', 'line 2', 'line 3', 'line 4', 'line 5'].join('\n');
const pages = [
    [undefined, undefined], [1, undefined], [undefined, 2], [2, 2], [4, 10], [5, 1], [6, 1], [100, 5],
    [0, 1], [-1, 1], [1, 0], [1, 10000], [1, 10001], [1.5, 1], [1, 2.5], ['2', 1], [1, '2'], [null, 2],
    [2, null], [null, null], [1e300, 1], [true, 1], [1, [2]], [-0, 1],
];

const json = [
    '{"b": 1, "a": 2}', '{"x": 1.0, "y": 1e2, "z": -0, "w": 12345678901234567890}', '{"s": "a\\u2028b<>&\\u0001\\"\\\\"}',
    '{"nested": {"k": [1, "two", null, true, {"q": false}]}}', '{"dup": 1, "other": 2, "dup": 3}', '{"big": 1e400}',
    '{"unicode": "\\u00e9\\ud83d\\ude00"}', '[]', '{}', '{"tab": "\\t\\n\\r\\b\\f"}',
    '{"a":1,"10":2,"2":3,"01":4,"4294967295":5,"4294967294":6,"-1":7,"0":8}', '{"n": {"b": 1, "1": 2}}',
];

const out = {
    title_to_slug: titles.map(t => ({ in: t, out: docs.titleToSlug(t) })),
    validate_slug: slugs.map(s => ({ in: s, out: outcome(() => { docs.validateSlug(s); }) })),
    validate_slug_prefix: ['a/', 'a/b/', 'a//', '/a/'].map(s => ({ in: s, out: outcome(() => { docs.validateSlug(s, { allowTrailingSlash: true }); }) })),
    slug_to_kind: kinds.map(k => {
        // slugToKind is not exported; saveNote's kind is the only use, so
        // the expected kinds are written here and checked against the
        // Node source by reading it.
        return { in: k };
    }),
    paginate: pages.map(([o, l]) => ({
        offset: o === undefined ? 'undefined' : o,
        limit: l === undefined ? 'undefined' : l,
        out: (() => {
            try {
                return docs.paginateContent(content, o, l);
            } catch (err) {
                return { error: err.message, status: err.statusCode };
            }
        })(),
    })),
    paginate_content: content,
    stringify: json.map(j => ({ in: j, out: JSON.stringify(JSON.parse(j)) })),
    to_fixed: [0, 0.05, 0.25, 0.35, 1.25, 49.95, 50, 9.999, 0.0000095367431640625, 52428790 / 1048576, 100 / 1048576, -0.001, -0, -0.04, -1.25].map(x => ({ in: x, out: x.toFixed(1) })),
};

// slugToKind is module-private; recover it from the source so the golden
// is still Node's own code, not a copy.
const src = fs.readFileSync(path.join(api, 'services/documents.js'), 'utf8');
const fnSrc = src.slice(src.indexOf('function slugToKind'), src.indexOf('function titleToSlug'));
const slugToKind = new Function(fnSrc + '; return slugToKind;')();
out.slug_to_kind = kinds.map(k => ({ in: k, out: slugToKind(k) }));

fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify(out, null, 2) + '\n');
