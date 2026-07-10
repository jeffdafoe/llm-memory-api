// Run with: node --test (from node/api). Uses the built-in node:test runner.
//
// Covers escapeLikePattern — the LIKE-metacharacter escaping that keeps a
// caller-supplied slug prefix from acting as a wildcard (LLM-355). The SQL
// filtering and last_accessed stamping that use it are exercised end-to-end
// against the live database (see the ticket), not here — this file has no DB.

const { test } = require('node:test');
const assert = require('node:assert');
const { escapeLikePattern } = require('./memory');

test('a plain hyphenated slug prefix is unchanged', () => {
    // Actor slugs are hyphenated, so the common case must pass through verbatim.
    assert.strictEqual(escapeLikePattern('anne-walker/memory/'), 'anne-walker/memory/');
});

test('an underscore is escaped (it is a single-char LIKE wildcard)', () => {
    assert.strictEqual(escapeLikePattern('anne_walker/'), 'anne\\_walker/');
});

test('a percent is escaped (it is a multi-char LIKE wildcard)', () => {
    assert.strictEqual(escapeLikePattern('100%/'), '100\\%/');
});

test('a backslash is escaped first so it cannot re-arm another metacharacter', () => {
    // Input "\_" must become "\\\_": the backslash doubles, and the underscore
    // is independently escaped. If backslash were escaped last, "\_" would wrongly
    // collapse to a single escaped underscore.
    assert.strictEqual(escapeLikePattern('\\_'), '\\\\\\_');
});

test('mixed metacharacters are each escaped', () => {
    assert.strictEqual(escapeLikePattern('a%b_c\\d'), 'a\\%b\\_c\\\\d');
});

test('an empty string yields an empty string', () => {
    assert.strictEqual(escapeLikePattern(''), '');
});
