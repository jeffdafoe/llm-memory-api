// Tests for buildSoulUserMessage — the pure soul-prompt assembler in the
// shared-NPC soul service (LLM-199). Run with: node --test (from node/api).
// Uses the built-in node:test runner + node:assert, matching
// sim-conversation-distiller.test.js / dream.test.js — no test-framework dep.
//
// The synthesis flow (findDreamAgent + invokeAgent + reasoning-preamble guard)
// is deploy-exercised like the distiller's DB path; the prompt assembler is the
// new pure surface and the one that must stay faithful to the dream pipeline's
// soul-block section shape.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { buildSoulUserMessage } = require('./sim-soul');

const SEED = 'Lewis Walker, who lodges at the Wayfarer with Hannah Walker.';
const SNAPSHOT = '- [Jun 30] spoke: "Good morrow."\n- [Jun 30] sold bread to a stranger.';

test('first run (empty soul) marks it empty and frames an initial synthesis', () => {
    const msg = buildSoulUserMessage({
        characterDescription: SEED,
        currentSoul: '',
        daySnapshot: SNAPSHOT,
        day: '2026-06-30',
    });
    assert.match(msg, /## Character description\n\nLewis Walker/);
    assert.match(msg, /## Current soul document\n\n\(empty — first run\)/);
    assert.match(msg, /## Dream snapshot for 2026-06-30\n\nThe current soul document is empty\. Synthesize an initial soul/);
    // The day material still lands at the end.
    assert.ok(msg.endsWith(SNAPSHOT));
});

test('missing/whitespace soul is treated as a first run', () => {
    const fromUndefined = buildSoulUserMessage({
        characterDescription: SEED, currentSoul: undefined, daySnapshot: SNAPSHOT, day: '2026-06-30',
    });
    const fromBlank = buildSoulUserMessage({
        characterDescription: SEED, currentSoul: '   \n  ', daySnapshot: SNAPSHOT, day: '2026-06-30',
    });
    for (const msg of [fromUndefined, fromBlank]) {
        assert.match(msg, /\(empty — first run\)/);
        assert.match(msg, /Synthesize an initial soul/);
    }
});

test('incremental run includes the prior soul and omits the first-run framing', () => {
    const prior = 'I am Lewis, a careful keeper who measures every coin.';
    const msg = buildSoulUserMessage({
        characterDescription: SEED,
        currentSoul: prior,
        daySnapshot: SNAPSHOT,
        day: '2026-06-30',
    });
    assert.match(msg, new RegExp('## Current soul document\\n\\n' + prior.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
    assert.doesNotMatch(msg, /\(empty — first run\)/);
    assert.doesNotMatch(msg, /Synthesize an initial soul/);
    assert.match(msg, /## Dream snapshot for 2026-06-30\n\n- \[Jun 30\] spoke/);
});

test('absent day yields a bare snapshot header (no "for <day>")', () => {
    const msg = buildSoulUserMessage({
        characterDescription: SEED,
        currentSoul: 'prior soul',
        daySnapshot: SNAPSHOT,
    });
    assert.match(msg, /## Dream snapshot\n\n/);
    assert.doesNotMatch(msg, /## Dream snapshot for/);
});

test('inputs are trimmed at the section boundaries', () => {
    const msg = buildSoulUserMessage({
        characterDescription: '  ' + SEED + '  ',
        currentSoul: '  prior soul  ',
        daySnapshot: '  ' + SNAPSHOT + '  ',
        day: '2026-06-30',
    });
    assert.match(msg, /## Character description\n\nLewis Walker/);
    assert.match(msg, /## Current soul document\n\nprior soul\n\n/);
    assert.ok(msg.endsWith(SNAPSHOT));
    // No doubled blank padding from untrimmed inputs.
    assert.doesNotMatch(msg, /\n\n\n/);
});

test('the three anchors render in order: character → soul → snapshot', () => {
    const msg = buildSoulUserMessage({
        characterDescription: SEED,
        currentSoul: 'prior soul',
        daySnapshot: SNAPSHOT,
        day: '2026-06-30',
    });
    const iChar = msg.indexOf('## Character description');
    const iSoul = msg.indexOf('## Current soul document');
    const iSnap = msg.indexOf('## Dream snapshot');
    assert.ok(iChar >= 0 && iSoul > iChar && iSnap > iSoul);
});
