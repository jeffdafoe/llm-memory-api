// Run with: node --test (from node/api). Uses the built-in node:test runner.
//
// Covers escapeLikePattern — the LIKE-metacharacter escaping that keeps a
// caller-supplied slug prefix from acting as a wildcard (LLM-355) — and
// preprocessQuery's embed-budget clamp (LLM-238). The SQL filtering and
// last_accessed stamping are exercised end-to-end against the live database
// (see the ticket), not here — this file has no DB.

const { test } = require('node:test');
const assert = require('node:assert');
const { escapeLikePattern, preprocessQuery, MAX_EMBED_QUERY_CHARS } = require('./memory');

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

// --- preprocessQuery embed-budget clamp (LLM-238) ---
// OpenAI's embeddings endpoint 400s on inputs over 8192 tokens. A 25KB+ inbound
// message (a review diff mailed to a virtual agent) would otherwise reach embed()
// unclamped and drop the VA's entire RAG recall. The clamp must bound EVERY return
// path, including the two raw-query fallbacks that fire when filler-stripping empties
// the query.

test('an over-budget single-token query is clamped to the char budget', () => {
    // No whitespace, no filler — exercises the normal return path with a query that
    // survives stripping intact, so only the clamp keeps it in budget.
    const huge = 'a'.repeat(MAX_EMBED_QUERY_CHARS + 5000);
    assert.strictEqual(preprocessQuery(huge).length, MAX_EMBED_QUERY_CHARS);
});

test('an over-budget all-filler query stays clamped via the fallback path', () => {
    // 'the' is a filler word, so stripping empties the word list and preprocessQuery
    // falls back to returning the (now clamped) raw query. Regression guard: the
    // fallback must not leak the full oversized input.
    const huge = 'the '.repeat(Math.ceil((MAX_EMBED_QUERY_CHARS + 5000) / 4));
    assert.ok(preprocessQuery(huge).length <= MAX_EMBED_QUERY_CHARS);
});

test('a truncation landing inside a surrogate pair drops the dangling half', () => {
    // Budget-1 'a's push the cut point onto the first emoji, so a naive slice would
    // keep a lone high surrogate. The guard must drop it — no malformed trailing char.
    const query = 'a'.repeat(MAX_EMBED_QUERY_CHARS - 1) + '😀'.repeat(100);
    const out = preprocessQuery(query);
    assert.ok(out.length <= MAX_EMBED_QUERY_CHARS);
    assert.ok(!/[\uD800-\uDBFF]$/.test(out), 'must not end on a lone high surrogate');
});

test('an under-budget query is still filler-stripped unchanged', () => {
    // The clamp must not alter normal queries — same behavior as before LLM-238.
    assert.strictEqual(preprocessQuery('how does the auth middleware work'), 'auth middleware work');
});

test('a non-string query still yields empty (guard preserved)', () => {
    assert.strictEqual(preprocessQuery(null), '');
});
