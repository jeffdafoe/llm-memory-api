// Tests for buildDecayConditions — the half-life reader behind the nightly
// decay cleanup. Run with: node --test (from node/api). Uses the built-in
// node:test runner + node:assert, matching dream.test.js.
//
// These are pure-function tests: config.set writes the in-memory cache
// directly, and buildDecayConditions only assembles SQL text, so no database
// is involved.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { buildDecayConditions } = require('./cleanup');
const config = require('./config');

const HALF_LIFE_KEYS = [
    'search_decay_halflife_task',
    'search_decay_halflife_learning',
    'search_decay_halflife_note',
    'search_decay_halflife_conversation',
    'search_decay_halflife_dream',
    'search_decay_halflife_episodic',
    'search_decay_halflife_reflective',
];

// Every test starts from "nothing decays" so each one only has to set the keys
// it is actually about, and an unset key can never leak in from a prior test.
function silenceAllHalfLives() {
    for (const key of HALF_LIFE_KEYS) {
        config.set(key, '0');
    }
}

// The conditions are raw SQL strings; a type is present when some condition
// mentions it. Kind conditions read `d.kind = 'note'`, cognitive ones read
// `cognitive_type') = 'episodic'`.
function mentions(conditions, needle) {
    return conditions.some((c) => c.includes(needle));
}

test('a half-life of 0 produces no decay condition for that type', () => {
    silenceAllHalfLives();
    const { conditions } = buildDecayConditions(0.05);
    assert.deepEqual(conditions, []);
});

test('an explicit 0 disables decay for a cognitive type that has a non-zero default (LLM-584)', () => {
    // The defect this covers: episodic and reflective were read as
    // `parseFloat(x) || 90` / `|| 180`, so the 0 their own config rows document
    // as "0 = no decay" was falsy and became 90/180 again. The `halfLife <= 0`
    // skip never fired, and runDecayCleanup soft-deletes what these conditions
    // match — so the note kept decaying, and being deleted, on a half-life the
    // operator had switched off.
    silenceAllHalfLives();
    config.set('search_decay_halflife_episodic', '0');
    config.set('search_decay_halflife_reflective', '0');

    const { conditions, params } = buildDecayConditions(0.05);

    assert.deepEqual(conditions, []);
    // Only the threshold is bound — no half-life was pushed.
    assert.deepEqual(params, [0.05]);
});

test('silencing one cognitive type leaves the other intact', () => {
    silenceAllHalfLives();
    config.set('search_decay_halflife_episodic', '0');
    config.set('search_decay_halflife_reflective', '180');

    const { conditions, params } = buildDecayConditions(0.05);

    assert.equal(conditions.length, 1);
    assert.ok(mentions(conditions, "= 'reflective'"));
    assert.ok(!mentions(conditions, "= 'episodic'"));
    assert.deepEqual(params, [0.05, 180]);
});

test('a non-zero half-life still produces its condition, with the value bound', () => {
    silenceAllHalfLives();
    config.set('search_decay_halflife_note', '45');

    const { conditions, params } = buildDecayConditions(0.05);

    assert.equal(conditions.length, 1);
    assert.ok(mentions(conditions, "d.kind = 'note'"));
    assert.deepEqual(params, [0.05, 45]);
});

test('an absent, blank or unparseable half-life falls back rather than decaying', () => {
    // Kind half-lives fall back to 0 (no decay); the two cognitive ones fall
    // back to their documented defaults.
    silenceAllHalfLives();
    config.set('search_decay_halflife_note', '');
    config.set('search_decay_halflife_dream', 'soon');
    config.set('search_decay_halflife_episodic', undefined);

    const { conditions, params } = buildDecayConditions(0.05);

    assert.equal(conditions.length, 1);
    assert.ok(mentions(conditions, "= 'episodic'"));
    assert.deepEqual(params, [0.05, 90]);
});

test('a negative half-life never reaches the SQL', () => {
    // A negative half-life would invert the decay curve, so it must never be
    // bound. It is invalid input rather than a request, so it takes the
    // fallback — which means the outcome differs by key, and both are correct:
    // a kind half-life falls back to 0 and decays nothing, while episodic and
    // reflective fall back to their documented 90/180 and decay normally.
    //
    // That second case is a deliberate change of behaviour. The old `|| 180`
    // kept the -1 (truthy) and let the `halfLife <= 0` skip swallow it, so a
    // typo silently disabled cleanup for every reflective note while search
    // ranking — already on parseNonNegativeFinite — went on applying 180. The
    // two now agree, which is the point of the ticket.
    silenceAllHalfLives();
    config.set('search_decay_halflife_note', '-45');
    config.set('search_decay_halflife_reflective', '-1');

    const { conditions, params } = buildDecayConditions(0.05);

    assert.equal(conditions.length, 1);
    assert.ok(mentions(conditions, "= 'reflective'"));
    assert.ok(!mentions(conditions, "d.kind = 'note'"));
    assert.deepEqual(params, [0.05, 180]);
});

test('parameter placeholders stay aligned with the bound values', () => {
    // The condition builder hands out $2, $3, ... as it walks the two maps.
    // A drifting index would bind a half-life to the wrong condition and
    // silently change which notes get deleted.
    silenceAllHalfLives();
    config.set('search_decay_halflife_note', '45');
    config.set('search_decay_halflife_dream', '30');
    config.set('search_decay_halflife_episodic', '90');

    const { conditions, params } = buildDecayConditions(0.05);

    assert.deepEqual(params, [0.05, 45, 30, 90]);
    assert.ok(conditions.some((c) => c.includes("d.kind = 'note'") && c.includes('$2')));
    assert.ok(conditions.some((c) => c.includes("d.kind = 'dream'") && c.includes('$3')));
    assert.ok(conditions.some((c) => c.includes("= 'episodic'") && c.includes('$4')));
});
