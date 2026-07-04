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
const { narrateEvent } = require('./sim-conversation-distiller');

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
