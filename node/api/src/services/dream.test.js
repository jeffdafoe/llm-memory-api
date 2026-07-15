// Tests for buildNotesLog — the notes-mode (dream_source=notes, MEM-137)
// source-text assembler in the dream cron. Run with: node --test (from
// node/api). Uses the built-in node:test runner + node:assert, matching
// sim-conversation-distiller.test.js.
//
// buildNotesLog is the only new pure surface of ZBBS-WORK-391; the sourcing
// branch and first-run window live in SQL + cron flow and are exercised at
// deploy (the conversation-mode path is untouched by the change).

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { buildNotesLog, soulNeedsRebuild, buildSoulUserMessage } = require('./dream');

test('single note gets a slug+date header above its content', () => {
    const rows = [{
        slug: 'journal/2026-05-california-trip',
        content: 'We drove the coast road.',
        updated_at: new Date('2026-05-18T14:22:00Z'),
    }];
    assert.equal(
        buildNotesLog(rows),
        '## Note: journal/2026-05-california-trip (updated 2026-05-18)\n\nWe drove the coast road.'
    );
});

test('multiple notes are separated and keep their query order', () => {
    const rows = [
        { slug: 'core-identity', content: 'A', updated_at: new Date('2026-05-16T08:00:00Z') },
        { slug: 'thea-core', content: 'B', updated_at: new Date('2026-05-16T19:30:00Z') },
    ];
    assert.equal(
        buildNotesLog(rows),
        '## Note: core-identity (updated 2026-05-16)\n\nA'
        + '\n\n---\n\n'
        + '## Note: thea-core (updated 2026-05-16)\n\nB'
    );
});

test('string updated_at (non-Date driver output) still renders the date', () => {
    const rows = [{
        slug: 'sirius-identity',
        content: 'C',
        updated_at: '2026-06-06T03:15:00.000Z',
    }];
    assert.equal(
        buildNotesLog(rows),
        '## Note: sirius-identity (updated 2026-06-06)\n\nC'
    );
});

// soulNeedsRebuild — the read-side rebuild trigger (LLM-420). Routes an empty
// OR suspiciously short soul through the from-scratch rebuild path so a
// truncated/degraded stub cannot be fed back in and compounded.
const HEALTHY_SOUL = 'x'.repeat(5000);

test('empty soul needs a rebuild', () => {
    assert.equal(soulNeedsRebuild('', 800), true);
});

test('whitespace-only soul needs a rebuild', () => {
    assert.equal(soulNeedsRebuild('   \n\t  ', 800), true);
});

test('null/undefined soul needs a rebuild (defensive)', () => {
    assert.equal(soulNeedsRebuild(null, 800), true);
    assert.equal(soulNeedsRebuild(undefined, 800), true);
});

test('a short stub below the floor needs a rebuild', () => {
    // The observed degraded stub was 708 chars; with an 800 floor it reroutes.
    assert.equal(soulNeedsRebuild('x'.repeat(708), 800), true);
});

test('a healthy full-length soul is evolved, not rebuilt', () => {
    assert.equal(soulNeedsRebuild(HEALTHY_SOUL, 800), false);
});

test('length is measured after trimming surrounding whitespace', () => {
    // 700 real chars padded to >800 with whitespace still counts as a stub.
    const padded = '   ' + 'x'.repeat(700) + '\n'.repeat(200);
    assert.equal(soulNeedsRebuild(padded, 800), true);
});

test('a soul exactly at the floor is not below it (boundary)', () => {
    assert.equal(soulNeedsRebuild('x'.repeat(800), 800), false);
});

test('minChars <= 0 disables the short-stub arm — only empty triggers rebuild', () => {
    assert.equal(soulNeedsRebuild('x'.repeat(10), 0), false);
    assert.equal(soulNeedsRebuild('x'.repeat(10), -1), false);
    assert.equal(soulNeedsRebuild('', 0), true);
});

// buildSoulUserMessage — the rebuild-vs-evolve prompt assembler (LLM-420). The
// critical guarantee: on a rebuild the degraded/truncated stub must NOT appear
// in the writer's input, or it would compound on the next cycle.
const STUB = 'a degraded truncated stub soul';

test('rebuild with a backload omits the prior soul and uses rebuild framing', () => {
    const msg = buildSoulUserMessage({
        agentName: 'work',
        startupInstructions: '',
        existingSoul: STUB,
        needsRebuild: true,
        backloadDreams: '### dreams/2026-07-14-x\n\nyesterday material',
        chunkDate: '2026-07-15',
        dreamContent: 'today chunk',
    });
    assert.ok(!msg.includes(STUB), 'the degraded stub must not leak into the prompt');
    assert.ok(msg.includes('(none on file — rebuilding from recent dreams)'));
    assert.ok(msg.includes('## Dream snapshot for initial soul rebuild'));
    assert.ok(msg.includes('There is no usable prior soul document.'));
    assert.ok(msg.includes('yesterday material'));
});

test('rebuild without a backload still omits the stub and falls back to the day chunk', () => {
    const msg = buildSoulUserMessage({
        agentName: 'work',
        startupInstructions: '',
        existingSoul: STUB,
        needsRebuild: true,
        backloadDreams: null,
        chunkDate: '2026-07-15',
        dreamContent: 'today chunk',
    });
    assert.ok(!msg.includes(STUB), 'the degraded stub must not leak into the prompt');
    assert.ok(msg.includes('(none on file — rebuilding from recent dreams)'));
    assert.ok(msg.includes('## Dream snapshot for 2026-07-15'));
    assert.ok(msg.includes('today chunk'));
    assert.ok(!msg.includes('initial soul rebuild'));
});

test('evolve path feeds the existing soul plus the day chunk, no rebuild framing', () => {
    const soul = 'a healthy multi-paragraph soul';
    const msg = buildSoulUserMessage({
        agentName: 'work',
        startupInstructions: 'You are Work.',
        existingSoul: soul,
        needsRebuild: false,
        backloadDreams: null,
        chunkDate: '2026-07-15',
        dreamContent: 'today chunk',
    });
    assert.ok(msg.includes(soul));
    assert.ok(msg.includes('## Character description\n\nYou are Work.'));
    assert.ok(msg.includes('## Dream snapshot for 2026-07-15'));
    assert.ok(!msg.includes('rebuilding from recent dreams'));
    assert.ok(!msg.includes('initial soul rebuild'));
});

test('empty startup instructions produce no character-description section', () => {
    const msg = buildSoulUserMessage({
        agentName: 'work',
        startupInstructions: '',
        existingSoul: 'soul',
        needsRebuild: false,
        backloadDreams: null,
        chunkDate: '2026-07-15',
        dreamContent: 'today',
    });
    assert.ok(!msg.includes('## Character description'));
});
