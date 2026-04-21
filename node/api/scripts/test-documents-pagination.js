#!/usr/bin/env node
// Unit tests for read_note pagination + grep context/regex behavior.
//
// Covers:
//   - paginateContent: no-params (back-compat), 1-indexed offset, limit
//     defaulting, header format, out-of-range offset, validation errors,
//     CRLF and trailing-newline content.
//   - grepNotes: bounds-check path thrown before DB (no pool needed); plus
//     full behavior tests using an injected pool stub (context merging,
//     non-contiguous block markers, regex mode, context=0, specific params
//     override the `context` shortcut).
//
// No real DB, no network — pure function-under-test verification.
// Run: node scripts/test-documents-pagination.js
// Exits 0 on pass, 1 on any failure.

// Install a pool stub BEFORE requiring the service. grepNotes imports
// ../db at module load; intercepting require() here lets us feed synthetic
// docs to the per-line scan without a Postgres connection.
const Module = require('module');
const origRequire = Module.prototype.require;
let stubbedDocs = [];
Module.prototype.require = function(id) {
    if (id === '../db') {
        return {
            query: async () => ({ rows: stubbedDocs.map(d => ({
                id: 1,
                namespace: d.namespace || 'test',
                slug: d.slug || 'example',
                title: d.title || 'Example',
                content: d.content,
                updated_at: new Date()
            })) })
        };
    }
    return origRequire.apply(this, arguments);
};

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

    // Exponential-backtracking pattern rejected by safe-regex.
    // (a+)+ on a long non-matching string is the textbook ReDoS case.
    await assertThrows400Async(
        'redos-prone pattern rejected',
        () => grepNotes('(a+)+', 'home', 10, null, { regex: true }),
        'backtracking'
    );

    // Length-capped: 201 chars exceeds the 200 cap.
    await assertThrows400Async(
        'regex pattern > 200 chars rejected',
        () => grepNotes('a'.repeat(201), 'home', 10, null, { regex: true }),
        '200'
    );

    // A safe regex of maximum allowed length should pass. Use a pattern
    // that safe-regex accepts — plain alternation is fine.
    stubbedDocs = [{ content: 'hello world' }];
    try {
        await grepNotes('a|b|c', 'test', 10, null, { regex: true });
        pass('simple alternation accepted in regex mode');
    } catch (err) {
        fail('simple alternation accepted in regex mode', `unexpected throw: ${err.message}`);
    }

    // ---------- Boundary: context_before/after exactly 50 is allowed ----------

    stubbedDocs = [{ content: 'a\nb\nc' }];
    try {
        await grepNotes('b', 'test', 10, null, { contextBefore: 50, contextAfter: 50 });
        pass('context_before=50 allowed (boundary)');
    } catch (err) {
        fail('context_before=50 allowed (boundary)', `unexpected throw: ${err.message}`);
    }

    // ---------- grepNotes behavior with stub pool ----------
    //
    // 12-line content with matches on lines 3 and 8.
    stubbedDocs = [{
        content: 'line1\nline2\nfoo line3\nline4\nline5\nline6\nline7\nfoo again line8\nline9\nline10\nline11 zzz\nline12'
    }];

    // Default context (±2): matches at 3,8 pull in 1..5 and 6..10 — contiguous, no block marker.
    {
        const r = await grepNotes('foo', 'test', 10, null, {});
        assertEqual('default ±2 matchCount', r[0].matchCount, 2);
        const any = r[0].matches.some(m => m.newBlock);
        assertEqual('default ±2 contiguous (no newBlock)', any, false);
        assertDeepEqual(
            'default ±2 line numbers',
            r[0].matches.map(m => m.lineNumber),
            [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]
        );
    }

    // context=5 wider — union should cover all 12 lines, still contiguous.
    {
        const r = await grepNotes('foo', 'test', 10, null, { context: 5 });
        assertEqual('context=5 spans all 12 lines', r[0].matches.length, 12);
        assertEqual('context=5 still contiguous', r[0].matches.some(m => m.newBlock), false);
    }

    // Asymmetric before=0, after=1 — blocks are now non-contiguous (3-4 and 8-9).
    {
        const r = await grepNotes('foo', 'test', 10, null, { contextBefore: 0, contextAfter: 1 });
        assertDeepEqual(
            'asymmetric before=0 after=1 lines',
            r[0].matches.map(m => m.lineNumber),
            [3, 4, 8, 9]
        );
        const blockLines = r[0].matches.filter(m => m.newBlock).map(m => m.lineNumber);
        assertDeepEqual('asymmetric: newBlock on line 8', blockLines, [8]);
    }

    // context=0 returns only the match lines, no context.
    {
        const r = await grepNotes('foo', 'test', 10, null, { context: 0 });
        assertEqual('context=0: only matches returned', r[0].matches.length, 2);
        const allMatches = r[0].matches.every(m => m.isMatch);
        assertEqual('context=0: all returned lines are matches', allMatches, true);
    }

    // Regex mode: odd-digit line endings on a single-line basis.
    {
        const r = await grepNotes('line[13579]$', 'test', 10, null, { regex: true, context: 0 });
        assertDeepEqual(
            'regex: matched only odd-ending lines',
            r[0].matches.map(m => m.lineNumber),
            [1, 3, 5, 7, 9]
        );
    }

    // Specific param wins over `context` shortcut. contextBefore=10 + context=3
    // should apply 10 before (clamped to line 1), not 3.
    {
        const r = await grepNotes('foo again', 'test', 10, null, { contextBefore: 10, context: 3 });
        const minLine = Math.min(...r[0].matches.map(m => m.lineNumber));
        // Match is at line 8; before=10 reaches back before line 1 → clamped to 1.
        assertEqual('specific contextBefore wins over context shortcut', minLine, 1);
    }

    // Non-contiguous regex matches produce `--` block markers in consumer
    // formatter. Verify the `newBlock` flag appears on gaps.
    {
        stubbedDocs = [{
            content: 'aaa\nfoo\nbbb\nccc\nddd\neee\nfff\nfoo\nggg'
        }];
        const r = await grepNotes('foo', 'test', 10, null, { context: 1 });
        // matches at lines 2,8; with context=1: {1,2,3} and {7,8,9} — non-contiguous.
        assertDeepEqual(
            'context=1 non-contiguous: line numbers',
            r[0].matches.map(m => m.lineNumber),
            [1, 2, 3, 7, 8, 9]
        );
        const block = r[0].matches.find(m => m.newBlock);
        assertEqual('context=1 non-contiguous: newBlock on line 7', block && block.lineNumber, 7);
    }

    // CRLF content should split on \n and preserve \r in returned lines.
    {
        stubbedDocs = [{ content: 'alpha\r\nbeta\r\ngamma foo\r\ndelta' }];
        const r = await grepNotes('foo', 'test', 10, null, { context: 0 });
        assertEqual('CRLF: match found', r[0].matchCount, 1);
        assertEqual('CRLF: \\r preserved in line', r[0].matches[0].line, 'gamma foo\r');
    }

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
