// Tests for narrateEvent — the per-event renderer in the sim-conversation
// distiller. Run with: node --test (from node/api). Uses the built-in node:test
// runner + node:assert, so no test-framework dependency is added.
//
// Focus is the ZBBS-WORK-376 piece-4 change: the v2-native action_type names
// (spoke/paid/walked/delivered/consumed/took_break) render correctly, and the
// renamed verbs (spoke/paid/walked) stay identical to their v1 counterparts
// (speak/pay/move_to).

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { narrateEvent, normalizeSlugPrefix, collapseTrailingSlashes } = require('./sim-conversation-distiller');

const ACTOR = 'Ezekiel Crane';

test('spoke renders quoted dialogue, identical to v1 speak', () => {
    const payload = { text: 'Good morrow, neighbour.' };
    assert.equal(narrateEvent({ kind: 'spoke', payload }, ACTOR), '"Good morrow, neighbour."');
    assert.equal(narrateEvent({ kind: 'speak', payload }, ACTOR), '"Good morrow, neighbour."');
});

test('spoke with empty text returns null (no line)', () => {
    assert.equal(narrateEvent({ kind: 'spoke', payload: { text: '   ' } }, ACTOR), null);
    assert.equal(narrateEvent({ kind: 'spoke', payload: {} }, ACTOR), null);
});

test('paid renders the transaction, identical to v1 pay', () => {
    const payload = { recipient: 'Hannah', amount: 3, for: 'bread' };
    assert.equal(narrateEvent({ kind: 'paid', payload }, ACTOR), '(paid Hannah 3 coins for bread)');
    assert.equal(narrateEvent({ kind: 'pay', payload }, ACTOR), '(paid Hannah 3 coins for bread)');
});

test('paid pluralizes coins and tolerates a missing for-text', () => {
    assert.equal(
        narrateEvent({ kind: 'paid', payload: { recipient: 'Hannah', amount: 1 } }, ACTOR),
        '(paid Hannah 1 coin)'
    );
    assert.equal(
        narrateEvent({ kind: 'paid', payload: { recipient: 'Hannah', amount: 0 } }, ACTOR),
        '(paid Hannah 0 coins)'
    );
});

test('walked renders the destination, identical to v1 move_to', () => {
    const payload = { destination: 'the Tavern' };
    assert.equal(narrateEvent({ kind: 'walked', payload }, ACTOR), '(walked to the Tavern)');
    assert.equal(narrateEvent({ kind: 'move_to', payload }, ACTOR), '(walked to the Tavern)');
});

test('delivered renders goods, recipient, and sale price', () => {
    assert.equal(
        narrateEvent({ kind: 'delivered', payload: { recipient: 'Bram', item: 'bread', qty: 2, amount: 6 } }, ACTOR),
        '(delivered 2x bread to Bram for 6 coins)'
    );
});

test('delivered omits the price when amount is zero, and qty 1 drops the multiplier', () => {
    assert.equal(
        narrateEvent({ kind: 'delivered', payload: { recipient: 'Bram', item: 'ale', qty: 1, amount: 0 } }, ACTOR),
        '(delivered ale to Bram)'
    );
});

test('consumed renders "had", with a quantity multiplier above one', () => {
    assert.equal(narrateEvent({ kind: 'consumed', payload: { item: 'ale', qty: 1 } }, ACTOR), '(had ale)');
    assert.equal(narrateEvent({ kind: 'consumed', payload: { item: 'bread', qty: 3 } }, ACTOR), '(had 3x bread)');
});

test('took_break renders the reason as an aside, or a bare line without one', () => {
    assert.equal(
        narrateEvent({ kind: 'took_break', payload: { reason: 'weary from the day' } }, ACTOR),
        '(stepped away, weary from the day)'
    );
    assert.equal(narrateEvent({ kind: 'took_break', payload: {} }, ACTOR), '(stepped away)');
});

test('labored renders the reward earned and the employer (LLM-162)', () => {
    assert.equal(
        narrateEvent({ kind: 'labored', payload: { employer: 'Hannah', amount: 5, duration_min: 30 } }, ACTOR),
        '(earned 5 coins working for Hannah)'
    );
    // amount 1 singularizes; a missing employer degrades to "someone".
    assert.equal(
        narrateEvent({ kind: 'labored', payload: { amount: 1 } }, ACTOR),
        '(earned 1 coin working for someone)'
    );
});

test('offered_work renders the employer-side job offer (LLM-564)', () => {
    assert.equal(
        narrateEvent({ kind: 'offered_work', payload: { worker: 'Patience', amount: 4, duration_min: 240 } }, ACTOR),
        '(offered Patience a job for 4 coins)'
    );
    // amount 1 singularizes; a missing worker degrades to "someone".
    assert.equal(
        narrateEvent({ kind: 'offered_work', payload: { amount: 1 } }, ACTOR),
        '(offered someone a job for 1 coin)'
    );
});

test('solicited_work renders the offer to the employer for the reward (LLM-213)', () => {
    assert.equal(
        narrateEvent({ kind: 'solicited_work', payload: { employer: 'Hannah', amount: 4, duration_min: 240 } }, ACTOR),
        '(offered to work for Hannah for 4 coins)'
    );
    // amount 1 singularizes; a missing employer degrades to "someone".
    assert.equal(
        narrateEvent({ kind: 'solicited_work', payload: { amount: 1 } }, ACTOR),
        '(offered to work for someone for 1 coin)'
    );
});

test('hired renders the worker taken on and the reward (LLM-213)', () => {
    assert.equal(
        narrateEvent({ kind: 'hired', payload: { worker: 'Hannah', amount: 4, duration_min: 240 } }, ACTOR),
        '(hired Hannah for 4 coins)'
    );
    // a missing worker degrades to "someone".
    assert.equal(
        narrateEvent({ kind: 'hired', payload: { amount: 6 } }, ACTOR),
        '(hired someone for 6 coins)'
    );
});

test('labor rewards carry the in-kind goods leg (LLM-225)', () => {
    // Goods + coins: the porridge leg joins the coin leg with "and".
    assert.equal(
        narrateEvent(
            {
                kind: 'labored',
                payload: { employer: 'Hannah', amount: 2, reward_items: [{ item: 'porridge', qty: 1 }] },
            },
            ACTOR
        ),
        '(earned porridge and 2 coins working for Hannah)'
    );
    // Goods-only (amount 0): the coin leg is dropped, not rendered "and 0 coins".
    assert.equal(
        narrateEvent(
            {
                kind: 'solicited_work',
                payload: { employer: 'Hannah', amount: 0, reward_items: [{ item: 'porridge', qty: 2 }] },
            },
            ACTOR
        ),
        '(offered to work for Hannah for 2x porridge)'
    );
    // Several goods lines join with commas before the coin leg.
    assert.equal(
        narrateEvent(
            {
                kind: 'hired',
                payload: {
                    worker: 'Anne',
                    amount: 1,
                    reward_items: [
                        { item: 'porridge', qty: 1 },
                        { item: 'bread', qty: 2 },
                    ],
                },
            },
            ACTOR
        ),
        '(hired Anne for porridge, 2x bread and 1 coin)'
    );
    // A malformed reward_items shape degrades to the coin leg (the
    // pre-LLM-225 rendering), never an empty phrase.
    assert.equal(
        narrateEvent({ kind: 'labored', payload: { employer: 'Hannah', amount: 5, reward_items: 'porridge' } }, ACTOR),
        '(earned 5 coins working for Hannah)'
    );
    // Malformed LINES are skipped too (code_review): empty item, qty 0,
    // non-numeric qty — none may render junk or suppress the coin fallback.
    assert.equal(
        narrateEvent(
            {
                kind: 'labored',
                payload: {
                    employer: 'Hannah',
                    amount: 5,
                    reward_items: [
                        { item: '', qty: 1 },
                        { item: 'porridge', qty: 0 },
                        { item: 'porridge', qty: 'x' },
                    ],
                },
            },
            ACTOR
        ),
        '(earned 5 coins working for Hannah)'
    );
    // A valid line still renders when it rides next to malformed ones.
    assert.equal(
        narrateEvent(
            {
                kind: 'labored',
                payload: {
                    employer: 'Hannah',
                    amount: 0,
                    reward_items: [
                        { item: '', qty: 1 },
                        { item: 'porridge', qty: 1 },
                    ],
                },
            },
            ACTOR
        ),
        '(earned porridge working for Hannah)'
    );
});

test('an unmapped kind is dropped from dreams, not surfaced as generic noise', () => {
    // LLM-283: loadDayEventsSQL pushes ALL of an actor's durable rows regardless
    // of type, so the old generic-narration default leaked feed/audit-only beats
    // into NPC dream memory. Unmapped kinds now drop to null. This covers
    // LLM-283's negotiation beats, the pre-existing gathered / stayed_open leaks,
    // and any future durable type until it earns an explicit mapping.
    assert.equal(narrateEvent({ kind: 'offered', payload: {} }, ACTOR), null);
    assert.equal(narrateEvent({ kind: 'declined', payload: {} }, ACTOR), null);
    assert.equal(narrateEvent({ kind: 'countered', payload: {} }, ACTOR), null);
    assert.equal(narrateEvent({ kind: 'gathered', payload: {} }, ACTOR), null);
    assert.equal(narrateEvent({ kind: 'stayed_open', payload: {} }, ACTOR), null);
    assert.equal(narrateEvent({ kind: 'summoned', payload: {} }, ACTOR), null);
    assert.equal(narrateEvent({ kind: 'some_future_beat', payload: {} }, ACTOR), null);
});

test('pure-perception kinds still render nothing', () => {
    assert.equal(narrateEvent({ kind: 'done', payload: {} }, ACTOR), null);
    assert.equal(narrateEvent({ kind: 'look_around', payload: {} }, ACTOR), null);
    assert.equal(narrateEvent({ kind: 'enter_huddle', payload: {} }, ACTOR), null);
});

// normalizeSlugPrefix (LLM-515) — the guard that keeps a shared-VA push's slug
// prefix from injecting anything unsafe into the saved note's slug. The engine
// derives the prefix from Slugify(displayName) + '/', so a legitimate value is
// one-or-more lowercase-kebab segments with a single trailing slash.
test('normalizeSlugPrefix accepts a canonical kebab prefix', () => {
    assert.equal(normalizeSlugPrefix('constance-scott/'), 'constance-scott/');
    assert.equal(normalizeSlugPrefix('john-ellis/'), 'john-ellis/');
    assert.equal(normalizeSlugPrefix('agent-7/'), 'agent-7/');
});

test('normalizeSlugPrefix canonicalizes a missing or doubled trailing slash', () => {
    assert.equal(normalizeSlugPrefix('constance-scott'), 'constance-scott/');
    assert.equal(normalizeSlugPrefix('constance-scott//'), 'constance-scott/');
    assert.equal(normalizeSlugPrefix('  constance-scott/  '), 'constance-scott/');
});

test('normalizeSlugPrefix rejects empty / non-string input as ""', () => {
    assert.equal(normalizeSlugPrefix(''), '');
    assert.equal(normalizeSlugPrefix('   '), '');
    assert.equal(normalizeSlugPrefix(null), '');
    assert.equal(normalizeSlugPrefix(undefined), '');
    assert.equal(normalizeSlugPrefix(42), '');
    assert.equal(normalizeSlugPrefix('/'), '');
});

test('normalizeSlugPrefix rejects an over-length prefix (slug/PK bound)', () => {
    // A well-formed but very long kebab string must not pass — it becomes part of
    // the saved note slug and the roster primary key. The cap is 100 chars
    // including the trailing slash.
    const under = 'a'.repeat(99) + '/'; // 100 chars → allowed
    const over = 'a'.repeat(100) + '/'; // 101 chars → rejected
    assert.equal(normalizeSlugPrefix(under), under);
    assert.equal(normalizeSlugPrefix(over), '');
    // The bound is on the CANONICAL value, not the raw input: extra trailing
    // slashes + whitespace push the raw length past 100 but collapse to a
    // 100-char canonical prefix, which is accepted.
    assert.equal(normalizeSlugPrefix('a'.repeat(99) + '////   '), 'a'.repeat(99) + '/');
    // A raw value still over 100 after canonicalization is rejected.
    assert.equal(normalizeSlugPrefix('a'.repeat(100) + '////'), '');
});

test('normalizeSlugPrefix rejects traversal and any non-kebab shape', () => {
    // Path traversal / nested paths — the whole reason the guard exists.
    assert.equal(normalizeSlugPrefix('../'), '');
    assert.equal(normalizeSlugPrefix('foo/../bar/'), '');
    assert.equal(normalizeSlugPrefix('/etc/passwd'), '');
    assert.equal(normalizeSlugPrefix('a/b/'), '');
    // Uppercase, dots, underscores, spaces — none are Slugify output.
    assert.equal(normalizeSlugPrefix('Constance-Scott/'), '');
    assert.equal(normalizeSlugPrefix('a.b/'), '');
    assert.equal(normalizeSlugPrefix('a_b/'), '');
    assert.equal(normalizeSlugPrefix('two words/'), '');
});

test('collapseTrailingSlashes is linear on a long run of slashes (LLM-583)', () => {
    // The collapse used to be `replace(/\/+$/, '')`. That pattern is unanchored at
    // the start, so on slashes NOT followed by end-of-string the engine restarts
    // at every offset and backtracks in O(n^2).
    //
    // This calls the helper directly rather than going through
    // normalizeSlugPrefix. It has to: the raw-length guard added alongside this
    // fix rejects anything past 1024 chars before the collapse is reached, so a
    // 200k-char input routed through normalizeSlugPrefix would short-circuit and
    // pass even with the quadratic regex restored. The linearity of the collapse
    // is a property worth pinning on its own — raising that ceiling must not
    // quietly reintroduce the backtracking.
    //
    // 200k slashes plus a trailing non-slash is the worst case. Measured on the
    // old code: 32,076ms. Measured on the new code: 0.005ms. The 5s bound sits
    // between them with 6x headroom against the quadratic case and six orders of
    // magnitude against the linear one, so a slow or GC-stalled runner cannot
    // flip it — only a return to backtracking can. Detecting backtracking at all
    // requires a clock; this is the least timing-sensitive form of that.
    const hostile = '/'.repeat(200000) + 'a';
    const startedAt = process.hrtime.bigint();
    assert.equal(collapseTrailingSlashes(hostile), hostile + '/');
    const elapsedMs = Number(process.hrtime.bigint() - startedAt) / 1e6;
    assert.ok(elapsedMs < 5000, `collapseTrailingSlashes took ${elapsedMs.toFixed(1)}ms — it is not linear`);
});

test('collapseTrailingSlashes matches the regex it replaced (LLM-583)', () => {
    // Semantic equivalence with `value.replace(/\/+$/, '') + '/'`, which is what
    // normalizeSlugPrefix used before. These are the shapes normalizeSlugPrefix
    // feeds it — post-trim, so no surrounding whitespace.
    assert.equal(collapseTrailingSlashes('constance-scott/'), 'constance-scott/');
    assert.equal(collapseTrailingSlashes('constance-scott'), 'constance-scott/');
    assert.equal(collapseTrailingSlashes('constance-scott////'), 'constance-scott/');
    assert.equal(collapseTrailingSlashes('/'), '/');
    assert.equal(collapseTrailingSlashes('////'), '/');
    assert.equal(collapseTrailingSlashes(''), '/');
    // Interior slashes are untouched — only a trailing run collapses. The kebab
    // regex in normalizeSlugPrefix is what rejects these.
    assert.equal(collapseTrailingSlashes('a/b'), 'a/b/');
    assert.equal(collapseTrailingSlashes('a/b//'), 'a/b/');
});

test('normalizeSlugPrefix rejects an oversized raw value before normalizing (LLM-583)', () => {
    // Defence in depth beside the linear collapse: the canonical cap is 100, so a
    // raw value past 1024 was never going to be accepted and is refused before any
    // trim/slice/regex touches it. The guard must not narrow what already passes —
    // the raw form legitimately carries whitespace and collapsing trailing slashes.
    assert.equal(normalizeSlugPrefix('/'.repeat(5 * 1024 * 1024)), '');
    assert.equal(normalizeSlugPrefix('a'.repeat(1025)), '');
    // Raw length over the canonical cap but under the raw ceiling still normalizes.
    assert.equal(normalizeSlugPrefix('a'.repeat(99) + '/'.repeat(500)), 'a'.repeat(99) + '/');
});
