#!/usr/bin/env node
// Standalone unit test for paraphraseToolCall (virtual-agent.js).
//
// paraphraseToolCall renders a prior assistant tool_call as a first-person line
// for the replayed assistant `content`, so the NPC's own actions are salient as
// language in history (the Llama-3.3 salience problem: it can't carry narration
// alongside a tool call, so content is emitted empty). The bug this fixes: the
// prior version checked move_to.destination — a field the engine never sends
// (it sends structure_name / structure_id) — and didn't handle pay_with_item /
// consume / take_break / stop at all, so those replayed as blank content and the
// NPC repeated itself (re-buying food turn after turn).
//
// Field shapes pinned here are the ACTUAL live engine tool args (verified from
// virtual_agent_calls on the running village).
//
// Run: node scripts/test-paraphrase-tool-call.js
// Exits 0 on pass, 1 on any failure.

const Module = require('module');
const origRequire = Module.prototype.require;

// Stub heavy deps so requiring virtual-agent.js doesn't open a DB pool. The
// function under test is pure; we only need the exported reference.
const stubExports = {};
Module.prototype.require = function (id) {
    if (id === '../db' || id === './db') return { query: async () => ({ rows: [] }) };
    if (/\/(notes|chat|mail|api-spend|metering|conversations|llm-clients|system-handler)$/.test(id)) {
        return new Proxy(stubExports, {
            get: function () { return function () { return Promise.resolve(null); }; }
        });
    }
    return origRequire.apply(this, arguments);
};

const { paraphraseToolCall } = require('../src/services/virtual-agent');

let passed = 0;
let failed = 0;
const failures = [];

function assertEqual(label, got, want) {
    if (got === want) {
        passed++;
    } else {
        failed++;
        failures.push('  FAIL [' + label + ']: got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want));
    }
}

// ---------- pay_with_item (the repeat-buying case) ----------

assertEqual(
    'pay_with_item consume_now=true (bool) → bought + consumed',
    paraphraseToolCall('pay_with_item', { qty: 1, item: 'Cheese', amount: 4, seller: 'John Ellis', consume_now: true }),
    '(I bought Cheese from John Ellis and consumed it)'
);

assertEqual(
    'pay_with_item consume_now="true" (Llama-stringified bool) → bought + consumed',
    paraphraseToolCall('pay_with_item', { item: 'Bread', seller: 'John Ellis', consume_now: 'true' }),
    '(I bought Bread from John Ellis and consumed it)'
);

assertEqual(
    'pay_with_item consume_now absent → bought only',
    paraphraseToolCall('pay_with_item', { item: 'Water', seller: 'Hannah Boggs' }),
    '(I bought Water from Hannah Boggs)'
);

assertEqual(
    'pay_with_item missing item → no paraphrase',
    paraphraseToolCall('pay_with_item', { seller: 'John Ellis', consume_now: true }),
    ''
);

// ---------- move_to (the field-name bug) ----------

assertEqual(
    'move_to structure_name → named',
    paraphraseToolCall('move_to', { structure_name: 'Tavern' }),
    '(I set off toward Tavern)'
);

assertEqual(
    'move_to structure_id (opaque UUID) → generic en-route line',
    paraphraseToolCall('move_to', { structure_id: '019dbcd2-c0b1-7bf9-98c2-0610cfb7f5e9' }),
    '(I set off walking to a place I could see)'
);

assertEqual(
    'move_to with the OLD (wrong) destination field → no paraphrase (regression guard)',
    paraphraseToolCall('move_to', { destination: 'Tavern' }),
    ''
);

// ---------- consume / take_break / stop / speak ----------

assertEqual('consume → consumed', paraphraseToolCall('consume', { qty: 1, item: 'Stew' }), '(I consumed Stew)');

assertEqual(
    'take_break with reason',
    paraphraseToolCall('take_break', { reason: 'I am exhausted' }),
    '(I stopped to take a break: I am exhausted)'
);

assertEqual('take_break no reason', paraphraseToolCall('take_break', {}), '(I stopped to take a break)');

assertEqual('stop (no args)', paraphraseToolCall('stop', {}), '(I stopped where I was)');

assertEqual(
    'speak quotes the text (escaped)',
    paraphraseToolCall('speak', { text: 'Good morrow, "friend".' }),
    '(I said aloud: "Good morrow, \\"friend\\".")'
);

// ---------- no-payload / unknown tools → '' ----------

assertEqual('done → empty', paraphraseToolCall('done', {}), '');
assertEqual('unknown tool → empty', paraphraseToolCall('scene_quote', { item_kind: 'Cheese' }), '');
assertEqual('null input → empty', paraphraseToolCall('stop', null), '(I stopped where I was)');
assertEqual('speak with empty text → empty', paraphraseToolCall('speak', { text: '' }), '');

// ---------- Report ----------

if (failed > 0) {
    console.log('FAILED ' + failed + ' / ' + (passed + failed));
    failures.forEach(function (f) { console.log(f); });
    process.exit(1);
}
console.log('PASS ' + passed + ' / ' + (passed + failed));
process.exit(0);
