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
const { buildNotesLog, soulNeedsRebuild, buildSoulUserMessage, extractSpeakers, buildPersonExcerptSections, buildPersonUserMessage, prefilterLog, runPersonContextUpdate, countFailedActors, peopleNotePath, validateRosterPrefix, resolveScopePrefix, planCronReport } = require('./dream');

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

// ─── LLM-523: ledger vs. talk in the people-VA prompt ───
//
// The defect these cover: extractSpeakers self-skips the villager's own lines
// (correct — she must not accumulate a people-file about herself), but EVERY
// ledger event in a sim-day note is spoken by the villager. So 100% of the
// engine's economic record was dropped before the people prompt was built, and
// the people-VA had only NPC-authored speech to go on. It duly wrote Josiah's
// unbacked "six coins to square us up" into durable memory as a completed
// payment, and turned a 1-coin milk PURCHASE into a gift.
//
// Fixture is the shape of Constance Scott's real 2026-07-15 and 2026-07-24
// days, trimmed to the lines that carry the defect.
const LEDGER_DAY = [
    '[Wednesday 17:48 Constance Scott] (offered to work for Josiah Thorne for 4 coins)',
    '[Wednesday 17:48 Josiah Thorne] "I can offer four coins for a couple of hours\' work, if that suits you."',
    '[Wednesday 19:49 Constance Scott] (earned 4 coins working for Josiah Thorne)',
    '[Wednesday 19:49 Constance Scott] (offered to work for Josiah Thorne for 6 coins)',
    '[Wednesday 20:23 Josiah Thorne] "Fifteen coins for three cuts — that\'s a fair trade, John."',
    '[Wednesday 20:24 John Ellis] "Good trades, Josiah. Appreciate you keeping the shelves stocked."',
    '[Wednesday 21:50 Constance Scott] (earned 6 coins working for Josiah Thorne)',
    '[Wednesday 22:00 Constance Scott] (had stew)',
    '[Wednesday 22:01 Josiah Thorne] "Off you go, then — safe to you, Constance Scott."',
].join('\n');

function sectionsFor(log, self) {
    return buildPersonExcerptSections(log, self, extractSpeakers(log, self));
}

test('the villager\'s own ledger lines are filed under the person they name', () => {
    const josiah = sectionsFor(LEDGER_DAY, 'Constance Scott').get('josiah-thorne');
    assert.deepEqual(josiah.ledger, [
        '[Wednesday 17:48 Constance Scott] (offered to work for Josiah Thorne for 4 coins)',
        '[Wednesday 19:49 Constance Scott] (earned 4 coins working for Josiah Thorne)',
        '[Wednesday 19:49 Constance Scott] (offered to work for Josiah Thorne for 6 coins)',
        '[Wednesday 21:50 Constance Scott] (earned 6 coins working for Josiah Thorne)',
    ]);
});

test('both engagements reach the prompt, so the day\'s full 10 coins are visible', () => {
    // The bug wrote "offered six coins for two hours shifting crates" and lost
    // the four-coin job entirely: the ledger was absent and the 4-coin offer had
    // also been dropped by the signal prefilter.
    const josiah = sectionsFor(LEDGER_DAY, 'Constance Scott').get('josiah-thorne');
    const text = josiah.ledger.join('\n');
    assert.ok(text.includes('earned 4 coins'), 'the four-coin engagement must survive');
    assert.ok(text.includes('earned 6 coins'), 'the six-coin engagement must survive');
});

test('a ledger line naming nobody is filed under nobody', () => {
    const sections = sectionsFor(LEDGER_DAY, 'Constance Scott');
    for (const [, entry] of sections) {
        assert.ok(
            !entry.ledger.some(l => l.includes('(had stew)')),
            'a solitary act is not a transaction with anyone'
        );
    }
});

test('the villager never gets a section about herself', () => {
    assert.ok(!sectionsFor(LEDGER_DAY, 'Constance Scott').has('constance-scott'));
});

// The 07-24 shape — the incident that raised this to High. Josiah SAID he'd
// paid ten coins and SAID he was handing over six to square up; the only thing
// that actually happened was Constance buying a jug of milk for one coin.
const PROMISE_DAY = [
    '[Friday 18:25 Josiah Thorne] "I paid you ten coins for your labor and added a cut of meat besides, as I recollect."',
    '[Friday 18:25 Josiah Thorne] "There we are — six coins to square us up, and a fair bit of gratitude besides."',
    '[Friday 18:26 Constance Scott] (paid Josiah Thorne 1 coin for milk)',
].join('\n');

test('an unbacked spoken promise produces no ledger entry', () => {
    const josiah = sectionsFor(PROMISE_DAY, 'Constance Scott').get('josiah-thorne');
    assert.deepEqual(josiah.ledger, ['[Friday 18:26 Constance Scott] (paid Josiah Thorne 1 coin for milk)']);
    assert.ok(
        !josiah.ledger.join('\n').includes('six coins'),
        'the six-coin restitution was spoken only — it must not appear as fact'
    );
    assert.ok(
        josiah.said.join('\n').includes('six coins to square us up'),
        'the promise still belongs in the file as something he said'
    );
});

test('the ledger keeps transaction direction — a purchase is not a gift', () => {
    const josiah = sectionsFor(PROMISE_DAY, 'Constance Scott').get('josiah-thorne');
    assert.ok(josiah.ledger[0].includes('(paid Josiah Thorne 1 coin for milk)'));
});

test('speech addressed to a third party is labeled overheard', () => {
    const josiah = sectionsFor(LEDGER_DAY, 'Constance Scott').get('josiah-thorne');
    const carrots = josiah.said.find(l => l.includes('Fifteen coins for three cuts'));
    assert.ok(carrots.endsWith('(overheard — addressed to John Ellis)'));
});

test('speech that names the villager is NOT labeled overheard', () => {
    const josiah = sectionsFor(LEDGER_DAY, 'Constance Scott').get('josiah-thorne');
    const farewell = josiah.said.find(l => l.includes('Off you go'));
    assert.ok(!farewell.includes('overheard'));
});

test('an ambiguous first name is not used to infer an addressee', () => {
    // Two Johns in the day: "John" alone identifies nobody, so the line stays
    // unlabeled rather than being attributed to the wrong man.
    const log = [
        '[Friday 12:00 Josiah Thorne] "Fair trade, John."',
        '[Friday 12:01 John Ellis] "Aye."',
        '[Friday 12:02 John Proctor] "Aye."',
    ].join('\n');
    const josiah = sectionsFor(log, 'Constance Scott').get('josiah-thorne');
    assert.ok(!josiah.said[0].includes('overheard'));
});

// prefilterLog: SIGNAL_PATTERNS are conversational markers, so a bare economic
// fact carries no signal word and used to survive only by luck of proximity.
test('a day of pure transactions is not discarded as signal-free', () => {
    // The early return used to fire whenever SIGNAL_PATTERNS found nothing,
    // taking the whole chunk with it — dream, learnings and people files. A day
    // where money moved and nobody chattered is precisely what this change
    // exists to carry through (code_review, LLM-523).
    const log = [
        '[Friday 18:26 Constance Scott] (paid Josiah Thorne 1 coin for milk)',
        '[Friday 19:49 Constance Scott] (earned 4 coins working for Josiah Thorne)',
    ].join('\n');
    const filtered = prefilterLog(log);
    assert.notEqual(filtered, null, 'a ledger-only day must survive the prefilter');
    assert.ok(filtered.includes('(paid Josiah Thorne 1 coin for milk)'));
    assert.ok(filtered.includes('(earned 4 coins working for Josiah Thorne)'));
});

test('a day with neither signal nor ledger is still discarded', () => {
    assert.equal(prefilterLog('the weather held\nthe road was dry'), null);
});

test('prefilterLog keeps a ledger line that carries no conversational signal', () => {
    const log = [
        'I want you to remember this.',
        'filler',
        'filler',
        'filler',
        'filler',
        'filler',
        '[Friday 20:03 Constance Scott] (delivered meat to John Ellis for 4 coins)',
    ].join('\n');
    assert.ok(prefilterLog(log).includes('(delivered meat to John Ellis for 4 coins)'));
});

test('a ledger line from another speaker is filed under that speaker', () => {
    // Not a shape the engine currently pushes, but a legal one. The two passes
    // read the same filtered text, so extractSpeakers has already created the
    // section this lands in.
    const log = [
        '[Friday 12:00 Josiah Thorne] (delivered 3x flour to Abraham Warren for 6 coins)',
        '[Friday 12:01 Constance Scott] "A fair price."',
    ].join('\n');
    const josiah = sectionsFor(log, 'Constance Scott').get('josiah-thorne');
    assert.deepEqual(josiah.ledger, ['[Friday 12:00 Josiah Thorne] (delivered 3x flour to Abraham Warren for 6 coins)']);
    assert.equal(josiah.said.length, 0, 'a ledger line is never repeated as speech');
});

test('an unpadded hour and a weekday-less header still attribute (LEDGER_LINE is the gate)', () => {
    // The header parser must not be stricter than LEDGER_LINE — a shape it
    // can't read would silently drop an authoritative transaction.
    const log = [
        '[Friday 9:05 Constance Scott] (paid Josiah Thorne 2 coins for bread)',
        '[18:26 Constance Scott] (paid Josiah Thorne 1 coin for milk)',
        '[Friday 12:01 Josiah Thorne] "Good day."',
    ].join('\n');
    const josiah = sectionsFor(log, 'Constance Scott').get('josiah-thorne');
    assert.equal(josiah.ledger.length, 2);
});

test('a name is matched on whole words, not substrings', () => {
    const log = [
        '[Friday 12:00 Josiah Thorne] "The annexed field is Anne\'s no longer."',
        '[Friday 12:01 Anne Walker] "Aye."',
    ].join('\n');
    const josiah = sectionsFor(log, 'Constance Scott').get('josiah-thorne');
    // "Anne's" is a real mention (apostrophe is a boundary); "annexed" is not,
    // and on its own would not have triggered the label.
    assert.ok(josiah.said[0].includes('(overheard — addressed to Anne Walker)'));
});

test('naming a third party while addressing the current person is not overheard', () => {
    // The complement of the labeling test: the villager is named, so the line
    // reads as spoken to her even though it discusses someone else.
    const log = [
        '[Friday 12:00 Josiah Thorne] "Constance, John Ellis still owes me two coins."',
        '[Friday 12:01 John Ellis] "I do not."',
    ].join('\n');
    const josiah = sectionsFor(log, 'Constance Scott').get('josiah-thorne');
    assert.ok(!josiah.said[0].includes('overheard'));
});

// The assembled prompt. Split out of runPersonContextUpdate precisely so this
// is assertable — that function reaches invokeAgent/readNote through
// destructured requires that can't be stubbed.
test('the user message renders ledger and speech as separate sections', () => {
    const msg = buildPersonUserMessage({
        selfLabel: 'Constance Scott',
        display: 'Josiah Thorne',
        today: '2026-07-24',
        existingFile: '',
        ledger: ['[Friday 18:26 Constance Scott] (paid Josiah Thorne 1 coin for milk)'],
        said: '[Friday 18:25 Josiah Thorne] "six coins to square us up"',
    });
    assert.ok(msg.includes('## What the ledger records\n\n[Friday 18:26 Constance Scott] (paid Josiah Thorne 1 coin for milk)'));
    assert.ok(msg.includes('## What was said in your hearing\n\n[Friday 18:25 Josiah Thorne] "six coins to square us up"'));
    assert.ok(msg.indexOf('## What the ledger records') < msg.indexOf('## What was said in your hearing'));
    assert.ok(msg.includes('Where the two disagree, the ledger wins.'));
});

test('an empty ledger says so explicitly rather than rendering blank', () => {
    const msg = buildPersonUserMessage({
        selfLabel: 'Constance Scott',
        display: 'Josiah Thorne',
        today: '2026-07-24',
        existingFile: 'prior file',
        ledger: [],
        said: 'he said he would make it right',
    });
    assert.ok(msg.includes('(nothing — no coin or goods changed hands between you and Josiah Thorne today)'));
});

test('consolidation-only mode keeps its sentinel wording', () => {
    // /admin/dream/consolidate-people drives this mode by passing no excerpts;
    // the people-VA's system prompt keys off this exact phrasing.
    const msg = buildPersonUserMessage({
        selfLabel: 'salem-vendor',
        display: 'Josiah Thorne',
        today: '2026-07-24',
        existingFile: 'bloated file',
        ledger: undefined,
        said: '',
    });
    assert.ok(msg.includes('(no new excerpts since last update — please consolidate any redundant bullets if present, or return file unchanged if already tight)'));
});

test('a dedicated NPC agent slug is recognized as its own display name', () => {
    // A dedicated agent's selfName is the agent slug ('zbbs-josiah-thorne'),
    // not a display name — so the self-recognition has to slug-to-display it or
    // every line addressed to Josiah reads as third-party.
    const log = [
        '[Friday 12:00 Josiah Thorne] (paid John Ellis 3 coins for meat)',
        '[Friday 12:01 John Ellis] "Fair trade, Josiah."',
    ].join('\n');
    const ellis = sectionsFor(log, 'zbbs-josiah-thorne').get('john-ellis');
    assert.deepEqual(ellis.ledger, ['[Friday 12:00 Josiah Thorne] (paid John Ellis 3 coins for meat)']);
    assert.ok(!ellis.said[0].includes('overheard'), 'a line naming Josiah is addressed to Josiah');
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

// countFailedActors — the shared-VA run-level failure signal: how many
// villagers saw ANY failure (actor granularity — one villager counts once even
// with several failed chunks; per-villager plannedChunks/completedChunks carry
// the finer detail). The subtle case is a failed CHUNK: it lives inside an actor
// result's chunks[] array, not as an actor-level `error` field, so a naive
// actor-level filter would miss an actor whose day actually failed.
test('countFailedActors counts an actor-level error (invalid prefix / exception)', () => {
    assert.equal(countFailedActors([{ prefix: 'bad/', error: 'invalid slug prefix' }]), 1);
});

test('countFailedActors counts an actor whose chunk failed (the round-2 regression)', () => {
    const actors = [{
        prefix: 'constance-scott/',
        plannedChunks: 2,
        completedChunks: 1,
        chunks: [
            { processed: true, chunkDate: '2026-07-15' },
            { chunkDate: '2026-07-16', error: 'model timeout' },
        ],
    }];
    assert.equal(countFailedActors(actors), 1);
});

test('countFailedActors ignores clean actors and skipped (non-error) chunks', () => {
    const actors = [
        { prefix: 'a/', plannedChunks: 1, completedChunks: 1, chunks: [{ processed: true }] },
        { prefix: 'b/', skipped: true, reason: 'no conversation notes' },
        { prefix: 'c/', plannedChunks: 1, completedChunks: 1, chunks: [{ skipped: true, reason: 'no signals' }] },
    ];
    assert.equal(countFailedActors(actors), 0);
});

test('countFailedActors counts each failed actor once across a roster (actor granularity)', () => {
    const actors = [
        { prefix: 'a/', error: 'invalid slug prefix' },              // actor-level error
        { prefix: 'b/', chunks: [{ processed: true }, { error: 'x' }] }, // one failed chunk
        { prefix: 'c/', chunks: [{ processed: true }] },             // clean
    ];
    assert.equal(countFailedActors(actors), 2);
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

test('peopleNotePath throws (builds no path) on an invalid prefix', () => {
    // A malformed prefix must not silently produce an unscoped or traversing
    // path — the throw happens before any concatenation.
    assert.throws(() => peopleNotePath(42, 'jeff'), /not a string/);
    assert.throws(() => peopleNotePath('../evil/', 'jeff'), /invalid slug prefix/);
    assert.throws(() => peopleNotePath('a%b/', 'jeff'), /invalid slug prefix/);
});

test('peopleNotePath validates the person slug too (both components)', () => {
    // The person slug is the OTHER path component — a raw '../secrets' would
    // traverse out of context/people/. Must throw, not build the path.
    assert.throws(() => peopleNotePath('', '../secrets'), /invalid person slug/);
    assert.throws(() => peopleNotePath('constance-scott/', '../secrets'), /invalid person slug/);
    assert.throws(() => peopleNotePath('', 'Not A Slug'), /invalid person slug/);
    assert.throws(() => peopleNotePath('', 42), /invalid person slug/);
    // A canonical slug still passes through unchanged.
    assert.equal(peopleNotePath('', 'josiah-thorne'), 'context/people/josiah-thorne');
});

// resolveScopePrefix — the single validated boundary (LLM-519 round 5). It must
// distinguish ABSENT (undefined/null/'' → dedicated namespace root) from
// PRESENT-BUT-INVALID (throws), so a malformed shared prefix can never silently
// collapse into the unscoped namespace.
test('resolveScopePrefix returns "" for an absent prefix', () => {
    assert.equal(resolveScopePrefix(undefined), '');
    assert.equal(resolveScopePrefix(null), '');
    assert.equal(resolveScopePrefix(''), '');
});

test('resolveScopePrefix accepts an already-canonical prefix unchanged', () => {
    assert.equal(resolveScopePrefix('constance-scott/'), 'constance-scott/');
    assert.equal(resolveScopePrefix('agent-7/'), 'agent-7/');
});

test('resolveScopePrefix rejects non-canonical forms rather than silently canonicalizing', () => {
    // normalizeSlugPrefix would canonicalize these on WRITE; the read boundary
    // requires them to be already canonical so distinct values can't collapse.
    assert.throws(() => resolveScopePrefix('john-ellis'), /invalid slug prefix/);        // missing slash
    assert.throws(() => resolveScopePrefix('constance-scott//'), /invalid slug prefix/); // doubled slash
    assert.throws(() => resolveScopePrefix('  constance-scott/  '), /invalid slug prefix/); // whitespace
});

test('resolveScopePrefix enforces the 100-char limit at the boundary', () => {
    // The security boundary now depends on the normalizer's length semantics, so
    // pin the edge directly: 100 chars (99 body + '/') is accepted, 101 rejected.
    const atLimit = 'a'.repeat(99) + '/';
    const overLimit = 'a'.repeat(100) + '/';
    assert.equal(atLimit.length, 100);
    assert.equal(resolveScopePrefix(atLimit), atLimit);
    assert.throws(() => resolveScopePrefix(overLimit), /invalid slug prefix/);
});

test('resolveScopePrefix throws on a present non-string prefix (no silent "")', () => {
    assert.throws(() => resolveScopePrefix(42), /not a string/);
    assert.throws(() => resolveScopePrefix({}), /not a string/);
    assert.throws(() => resolveScopePrefix([]), /not a string/);
    assert.throws(() => resolveScopePrefix(true), /not a string/);
});

test('resolveScopePrefix throws on a non-canonical string (wildcard / traversal)', () => {
    assert.throws(() => resolveScopePrefix('a%b/'), /invalid slug prefix/);
    assert.throws(() => resolveScopePrefix('a_b/'), /invalid slug prefix/);
    assert.throws(() => resolveScopePrefix('../secrets/'), /invalid slug prefix/);
    assert.throws(() => resolveScopePrefix('foo/../bar/'), /invalid slug prefix/);
    assert.throws(() => resolveScopePrefix('Constance-Scott/'), /invalid slug prefix/);
});

// planCronReport — the scheduler's status mapping. The completion event's
// `status` field is the authoritative, self-contained failure signal (no
// cross-event persistence ordering is relied upon); a shared-failures event is
// additionally emitted for error_log monitoring when a shared actor failed.
test('planCronReport marks a clean run "ok" with only a completion event', () => {
    assert.deepEqual(planCronReport({ failedSharedActorCount: 0 }), [
        { kind: 'complete', status: 'ok' },
    ]);
    assert.deepEqual(planCronReport(null), [{ kind: 'complete', status: 'ok' }]);
});

test('planCronReport marks a failed run "completed-with-errors" and adds a failure event', () => {
    const events = planCronReport({ failedSharedActorCount: 3 });
    const complete = events.find(e => e.kind === 'complete');
    const failure = events.find(e => e.kind === 'shared-failures');
    // The completion record self-describes the failure via its status field.
    assert.deepEqual(complete, { kind: 'complete', status: 'completed-with-errors' });
    // A distinct failure event is also emitted (for error_log monitoring).
    assert.deepEqual(failure, { kind: 'shared-failures', count: 3 });
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

test('validateRosterPrefix accepts an already-canonical prefix', () => {
    assert.equal(validateRosterPrefix('constance-scott/'), 'constance-scott/');
});

test('validateRosterPrefix rejects a non-canonical prefix (must be stored canonical)', () => {
    // The distiller stores canonical values; the read boundary rejects drift
    // rather than silently canonicalizing it, so distinct values can't collapse.
    assert.equal(validateRosterPrefix('john-ellis'), null);       // missing slash
    assert.equal(validateRosterPrefix('constance-scott//'), null); // doubled slash
    assert.equal(validateRosterPrefix('  constance-scott/  '), null); // whitespace
});
