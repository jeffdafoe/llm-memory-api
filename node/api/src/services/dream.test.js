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
const { buildNotesLog, soulNeedsRebuild, buildSoulUserMessage, extractSpeakers, runPersonContextUpdate, countActorErrors, peopleNotePath, validateRosterPrefix } = require('./dream');

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
    // The shared length cap (LLM-501) closes the message — same directive the
    // sim-soul endpoint appends, so both soul writers carry one contract.
    const { SOUL_LENGTH_DIRECTIVE } = require('./sim-soul');
    assert.ok(msg.endsWith(SOUL_LENGTH_DIRECTIVE));
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

// extractSpeakers self-skip under a shared-VA scope (LLM-519 Slice 2). The
// dream cron threads the villager's display name as the self-identity so her
// own sim-day lines are dropped (no self people-file) while counterparties are
// kept. A shared villager's lines come through the sim distiller format
// ([Weekday HH:MM Display Name] ...), and the display name is multi-word.
const SIM_DAY_LOG = [
    '[Wednesday 14:30 Constance Scott] (earned 6 coins working for Josiah Thorne)',
    '[Wednesday 14:35 Josiah Thorne] Fair work, fairly paid.',
].join('\n');

test('shared-VA scope skips the villager\'s own sim-day lines, keeps the counterparty', () => {
    const speakers = extractSpeakers(SIM_DAY_LOG, 'Constance Scott');
    assert.ok(!speakers.has('constance-scott'), 'the villager herself must be self-skipped');
    assert.ok(speakers.has('josiah-thorne'), 'the counterparty must be captured');
    assert.equal(speakers.get('josiah-thorne').display, 'Josiah Thorne');
});

test('the pooled agent name does NOT self-skip the villager — why selfName threading is needed', () => {
    // Passing the pooled agent (salem-vendor) as self, the pre-Slice-2 default,
    // fails to match the villager's own lines: she'd wrongly accumulate a
    // context/people file about herself. Slice 2 fixes this by passing her
    // display name as selfName.
    const speakers = extractSpeakers(SIM_DAY_LOG, 'salem-vendor');
    assert.ok(speakers.has('constance-scott'), 'pooled agent name leaves the villager un-skipped');
});

// runPersonContextUpdate rejects a non-canonical slug prefix before touching
// the store (LLM-519 code_review): the prefix reaches note paths and LIKE
// patterns, so a '%'/'_' wildcard or a '../' traversal must be refused at this
// exported boundary, not trusted from the roster row. The guard runs before any
// readNote/invokeAgent call, so these assert only the synchronous rejection.
test('runPersonContextUpdate rejects a LIKE-wildcard slug prefix', async () => {
    await assert.rejects(
        () => runPersonContextUpdate('salem-vendor', 'dream-sim-people', 'josiah-thorne', 'Josiah Thorne', 'x', '2026-07-15', { slugPrefix: 'cons%tance/' }),
        /invalid slug prefix/
    );
});

test('runPersonContextUpdate rejects a path-traversal slug prefix', async () => {
    await assert.rejects(
        () => runPersonContextUpdate('salem-vendor', 'dream-sim-people', 'josiah-thorne', 'Josiah Thorne', 'x', '2026-07-15', { slugPrefix: '../secrets/' }),
        /invalid slug prefix/
    );
});

// countActorErrors — the shared-VA run-level failure signal (LLM-519 round 2).
// The subtle case is a failed CHUNK: it lives inside an actor result's chunks[]
// array, not as an actor-level `error` field, so a naive actor-level filter
// would report a clean run for an actor whose day actually failed.
test('countActorErrors counts an actor-level error (invalid prefix / exception)', () => {
    assert.equal(countActorErrors([{ prefix: 'bad/', error: 'invalid slug prefix' }]), 1);
});

test('countActorErrors counts an actor whose chunk failed (the round-2 regression)', () => {
    const actors = [{
        prefix: 'constance-scott/',
        plannedChunks: 2,
        completedChunks: 1,
        chunks: [
            { processed: true, chunkDate: '2026-07-15' },
            { chunkDate: '2026-07-16', error: 'model timeout' },
        ],
    }];
    assert.equal(countActorErrors(actors), 1);
});

test('countActorErrors ignores clean actors and skipped (non-error) chunks', () => {
    const actors = [
        { prefix: 'a/', plannedChunks: 1, completedChunks: 1, chunks: [{ processed: true }] },
        { prefix: 'b/', skipped: true, reason: 'no conversation notes' },
        { prefix: 'c/', plannedChunks: 1, completedChunks: 1, chunks: [{ skipped: true, reason: 'no signals' }] },
    ];
    assert.equal(countActorErrors(actors), 0);
});

test('countActorErrors sums actor-level and chunk-level failures across a roster', () => {
    const actors = [
        { prefix: 'a/', error: 'invalid slug prefix' },
        { prefix: 'b/', chunks: [{ processed: true }, { error: 'x' }] },
        { prefix: 'c/', chunks: [{ processed: true }] },
    ];
    assert.equal(countActorErrors(actors), 2);
});

// peopleNotePath is the exact path runPersonContextUpdate reads and writes, so
// asserting it directly proves the dedicated/admin empty-prefix invariant
// (context/people/<slug> at namespace root) and the shared-VA scoping without
// mocking the document store — which the destructured-import style resists.
test('peopleNotePath builds a namespace-root path for an empty prefix (dedicated/admin)', () => {
    assert.equal(peopleNotePath('', 'jeff'), 'context/people/jeff');
});

test('peopleNotePath treats a missing prefix as empty', () => {
    assert.equal(peopleNotePath(undefined, 'jeff'), 'context/people/jeff');
});

test('peopleNotePath scopes the path under a shared-VA villager prefix', () => {
    assert.equal(
        peopleNotePath('constance-scott/', 'josiah-thorne'),
        'constance-scott/context/people/josiah-thorne'
    );
});

// validateRosterPrefix — the roster-boundary decision (LLM-519 round 3). A
// NULL/non-string row must be rejected (returns null → the caller skips only
// that villager, never aborting the roster), as must any non-canonical value
// (LIKE metacharacters, path traversal). A canonical prefix passes through.
test('validateRosterPrefix rejects a NULL roster prefix', () => {
    assert.equal(validateRosterPrefix(null), null);
});

test('validateRosterPrefix rejects a non-string roster prefix', () => {
    assert.equal(validateRosterPrefix(42), null);
    assert.equal(validateRosterPrefix(undefined), null);
    assert.equal(validateRosterPrefix({}), null);
});

test('validateRosterPrefix rejects LIKE-wildcard and traversal prefixes', () => {
    assert.equal(validateRosterPrefix('cons%tance/'), null);
    assert.equal(validateRosterPrefix('a_b/'), null);
    assert.equal(validateRosterPrefix('../secrets/'), null);
    assert.equal(validateRosterPrefix('foo/../bar/'), null);
});

test('validateRosterPrefix accepts and canonicalizes a valid prefix', () => {
    assert.equal(validateRosterPrefix('constance-scott/'), 'constance-scott/');
    // Canonicalizes a missing trailing slash (defensive; distiller writes it).
    assert.equal(validateRosterPrefix('john-ellis'), 'john-ellis/');
});
