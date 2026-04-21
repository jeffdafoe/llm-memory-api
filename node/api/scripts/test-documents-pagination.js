#!/usr/bin/env node
// Unit tests for read_note pagination + grep bounds validation.
//
// Covers:
//   - paginateContent: no-params (back-compat), 1-indexed offset, limit defaulting,
//     header format, out-of-range offset, validation errors.
//   - grepNotes: bounds-check path for context_before / context_after (thrown
//     before the DB is touched, so no pool needed).
//
// No DB, no network — pure function-under-test verification.
// Run: node scripts/test-documents-pagination.js
// Exits 0 on pass, 1 on any failure.

const { paginateContent, grepNotes } = require('../src/services/documents');

let passed = 0;
let failed = 0;
const failures = [];

function pass(label) {
    passed++;
}
function fail(label, detail) {
    failed++;
    failures.push(`  FAIL [${label}] ${detail}`);
}

function assertEqual(label, actual, expected) {
    if (actual === expected) {
        pass(label);
    } else {
        fail(label, `expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)}`);
    }
}

function assertDeepEqual(label, actual, expected) {
    const a = JSON.stringify(actual);
    const e = JSON.stringify(expected);
    if (a === e) {
        pass(label);
    } else {
        fail(label, `expected ${e}, got ${a}`);
    }
}

function assertThrows400(label, fn, messageFragment) {
    try {
        fn();
        fail(label, 'expected throw, got no error');
    } catch (err) {
        if (err.statusCode !== 400) {
            fail(label, `expected statusCode 400, got ${err.statusCode}: ${err.message}`);
            return;
        }
        if (messageFragment && !err.message.includes(messageFragment)) {
            fail(label, `expected message to include "${messageFragment}", got "${err.message}"`);
            return;
        }
        pass(label);
    }
}

async function assertThrows400Async(label, fn, messageFragment) {
    try {
        await fn();
        fail(label, 'expected throw, got no error');
    } catch (err) {
        if (err.statusCode !== 400) {
            fail(label, `expected statusCode 400, got ${err.statusCode}: ${err.message}`);
            return;
        }
        if (messageFragment && !err.message.includes(messageFragment)) {
            fail(label, `expected message to include "${messageFragment}", got "${err.message}"`);
            return;
        }
        pass(label);
    }
}

// ---------- paginateContent: no-params back-compat ----------

const fiveLines = 'a\nb\nc\nd\ne';
{
    const r = paginateContent(fiveLines, undefined, undefined);
    assertEqual('no params: text unchanged', r.text, fiveLines);
    assertEqual('no params: paginated=false', r.paginated, false);
}
{
    const r = paginateContent(fiveLines, null, null);
    assertEqual('null params: text unchanged', r.text, fiveLines);
    assertEqual('null params: paginated=false', r.paginated, false);
}

// ---------- paginateContent: offset only ----------

{
    const r = paginateContent(fiveLines, 2, undefined);
    // offset=2, limit defaults to 2000, so returns lines 2-5 of 5
    assertEqual('offset only: header present', r.text.startsWith('[lines 2-5 of 5]\n\n'), true);
    assertEqual('offset only: body is lines 2-5', r.text.split('\n\n')[1], 'b\nc\nd\ne');
    assertEqual('offset only: paginated=true', r.paginated, true);
    assertEqual('offset only: totalLines=5', r.totalLines, 5);
}

// ---------- paginateContent: limit only ----------

{
    const r = paginateContent(fiveLines, undefined, 3);
    // limit=3, offset defaults to 1
    assertEqual('limit only: header', r.text.split('\n\n')[0], '[lines 1-3 of 5]');
    assertEqual('limit only: body', r.text.split('\n\n')[1], 'a\nb\nc');
}

// ---------- paginateContent: offset + limit ----------

{
    const r = paginateContent(fiveLines, 2, 2);
    assertEqual('offset+limit: header', r.text.split('\n\n')[0], '[lines 2-3 of 5]');
    assertEqual('offset+limit: body', r.text.split('\n\n')[1], 'b\nc');
}

// ---------- paginateContent: limit past end ----------

{
    const r = paginateContent(fiveLines, 4, 100);
    // Should clip to lines 4-5
    assertEqual('limit past end: header', r.text.split('\n\n')[0], '[lines 4-5 of 5]');
    assertEqual('limit past end: body', r.text.split('\n\n')[1], 'd\ne');
}

// ---------- paginateContent: offset past end ----------

{
    const r = paginateContent(fiveLines, 99, 10);
    assertEqual('offset past end: open-ended header', r.text, '[lines 99- of 5]\n\n');
    assertEqual('offset past end: paginated=true', r.paginated, true);
    assertEqual('offset past end: totalLines=5', r.totalLines, 5);
}

// ---------- paginateContent: single-line note ----------

{
    const r = paginateContent('only-line', 1, 10);
    assertEqual('single line: header', r.text.split('\n\n')[0], '[lines 1-1 of 1]');
    assertEqual('single line: body', r.text.split('\n\n')[1], 'only-line');
}

// ---------- paginateContent: validation errors ----------

assertThrows400('offset=0 rejects', () => paginateContent(fiveLines, 0, 10), 'offset');
assertThrows400('offset=-1 rejects', () => paginateContent(fiveLines, -1, 10), 'offset');
assertThrows400('offset=1.5 rejects', () => paginateContent(fiveLines, 1.5, 10), 'offset');
assertThrows400('limit=0 rejects', () => paginateContent(fiveLines, 1, 0), 'limit');
assertThrows400('limit=-5 rejects', () => paginateContent(fiveLines, 1, -5), 'limit');
assertThrows400('limit=10001 rejects', () => paginateContent(fiveLines, 1, 10001), 'limit');
assertThrows400('limit=1.5 rejects', () => paginateContent(fiveLines, 1, 1.5), 'limit');

// ---------- paginateContent: empty content ----------

{
    // Empty content splits to one empty line (`''`.split('\n') = ['']).
    const r = paginateContent('', undefined, undefined);
    assertEqual('empty no-params: raw', r.text, '');
    assertEqual('empty no-params: paginated=false', r.paginated, false);
}
{
    // With offset=1 on empty content: startLine 1 <= totalLines 1, so returns
    // header + empty body.
    const r = paginateContent('', 1, 10);
    assertEqual('empty + offset: header', r.text.split('\n\n')[0], '[lines 1-1 of 1]');
    assertEqual('empty + offset: body empty', r.text.split('\n\n')[1], '');
}

// ---------- grepNotes: validation errors (thrown before DB hit) ----------

(async () => {
    await assertThrows400Async(
        'missing pattern',
        () => grepNotes('', 'home', 10, null, {}),
        'pattern'
    );
    await assertThrows400Async(
        'context_before > 50',
        () => grepNotes('x', 'home', 10, null, { contextBefore: 51 }),
        'context_before'
    );
    await assertThrows400Async(
        'context_before negative',
        () => grepNotes('x', 'home', 10, null, { contextBefore: -1 }),
        'context_before'
    );
    await assertThrows400Async(
        'context_after > 50',
        () => grepNotes('x', 'home', 10, null, { contextAfter: 100 }),
        'context_after'
    );
    await assertThrows400Async(
        'context_before non-integer',
        () => grepNotes('x', 'home', 10, null, { contextBefore: 2.5 }),
        'context_before'
    );
    await assertThrows400Async(
        'context shortcut > 50 via context_before',
        () => grepNotes('x', 'home', 10, null, { context: 60 }),
        'context_before'
    );
    await assertThrows400Async(
        'invalid regex',
        () => grepNotes('[unclosed', 'home', 10, null, { regex: true }),
        'regex'
    );

    // ---------- Report ----------

    console.log(`\nRan ${passed + failed} cases: ${passed} passed, ${failed} failed`);
    if (failed > 0) {
        console.log('\nFailures:');
        for (const line of failures) console.log(line);
        process.exit(1);
    }
    console.log('All pagination/grep validation cases passed.');
    process.exit(0);
})();
