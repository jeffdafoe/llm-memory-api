// Tests for mergeRateLimit — the pure global-defaults + per-agent-override
// merge that effectiveRateLimit() feeds and isRateLimited() enforces (LLM-156).
// Run with: node --test (from node/api). Uses the built-in node:test runner +
// node:assert, matching dream.test.js / sim-conversation-distiller.test.js.
//
// Lives in its own pure module so the test loads neither the DB pool nor the
// virtual-agent stack (which registers a system handler at module load).

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { mergeRateLimit } = require('./virtual-agent-rate-limit');

const GLOBALS = { limit: 10, windowMs: 60000, cooldownMs: 300000 };

test('no overrides returns the globals unchanged', () => {
    assert.deepEqual(mergeRateLimit(GLOBALS, null), GLOBALS);
});

test('a full override replaces every field', () => {
    const overrides = { limit: 30, windowMs: 60000, cooldownMs: 60000 };
    assert.deepEqual(mergeRateLimit(GLOBALS, overrides), {
        limit: 30,
        windowMs: 60000,
        cooldownMs: 60000,
    });
});

test('each override field is independent — a limit-only override inherits window + cooldown', () => {
    const overrides = { limit: 40, windowMs: null, cooldownMs: null };
    assert.deepEqual(mergeRateLimit(GLOBALS, overrides), {
        limit: 40,
        windowMs: 60000,
        cooldownMs: 300000,
    });
});

test('a null override field falls through to the global (not coerced to 0)', () => {
    const overrides = { limit: null, windowMs: null, cooldownMs: 90000 };
    assert.deepEqual(mergeRateLimit(GLOBALS, overrides), {
        limit: 10,
        windowMs: 60000,
        cooldownMs: 90000,
    });
});

test('does not mutate the caller-supplied globals object', () => {
    const globals = { limit: 10, windowMs: 60000, cooldownMs: 300000 };
    mergeRateLimit(globals, { limit: 99, windowMs: 1, cooldownMs: 2 });
    assert.deepEqual(globals, { limit: 10, windowMs: 60000, cooldownMs: 300000 });
});
