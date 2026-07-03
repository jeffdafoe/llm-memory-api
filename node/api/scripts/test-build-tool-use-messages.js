#!/usr/bin/env node
// Standalone test for buildToolUseMessages (virtual-agent.js) — focused on the
// MEM-133 relative-time grounding: engine perceptions get a [age] prefix and a
// "--- gap: Nh ---" marker after a long pause, mirroring the companion path the
// sim tool-use branch had dropped. Also pins the role mapping + salience content.
//
// Run: node scripts/test-build-tool-use-messages.js   (exits 0 pass / 1 fail)

const Module = require('module');
const origRequire = Module.prototype.require;
const stubExports = {};
Module.prototype.require = function (id) {
    if (id === '../db' || id === './db') return { query: async () => ({ rows: [] }) };
    if (/\/(notes|chat|mail|api-spend|metering|conversations|llm-clients|system-handler)$/.test(id)) {
        return new Proxy(stubExports, { get: function () { return function () { return Promise.resolve(null); }; } });
    }
    return origRequire.apply(this, arguments);
};

const { buildToolUseMessages } = require('../src/services/virtual-agent');

let passed = 0, failed = 0;
const failures = [];
function check(label, cond) {
    if (cond) { passed++; } else { failed++; failures.push('  FAIL [' + label + ']'); }
}

const NOW = Date.now();
const agoISO = (mins) => new Date(NOW - mins * 60000).toISOString();
const NPC = 'zbbs-john-ellis';

// 3h-ago perception -> assistant pay_with_item -> [ok] -> 5m-ago perception
// (the ~2.9h gap between the tool row and the 2nd perception trips the marker).
const history = [
    { from_agent: 'salem-engine', message: '# Your turn\n## You\nstarving', sent_at: agoISO(180) },
    { from_agent: NPC, message: '', tool_calls: [{ id: 't1', name: 'pay_with_item', input: { item: 'Cheese', seller: 'Hannah', consume_now: true } }], sent_at: agoISO(179) },
    { from_agent: 'salem-engine', message: '[ok]', tool_call_id: 't1', sent_at: agoISO(179) },
    { from_agent: 'salem-engine', message: '# Your turn\n## You\nstill starving', sent_at: agoISO(5) },
];
const msgs = buildToolUseMessages(history, NPC);

check('4 messages', msgs.length === 4);
// 1st perception: [3h] prefix, no gap (first row).
check('msg0 is user', msgs[0].role === 'user');
// WORK-434 spelled out relative timestamps ([3h] → [3 hours ago]); these
// expectations were stale until LLM-237 touched this file.
check('msg0 prefixed with [3 hours ago]', msgs[0].content.startsWith('[3 hours ago]\n# Your turn'));
check('msg0 has no gap marker (first row)', !msgs[0].content.includes('gap:'));
// assistant: salience paraphrase, NO time prefix.
check('msg1 is assistant', msgs[1].role === 'assistant');
check('msg1 content is the offer paraphrase', msgs[1].content === '(I offered to buy Cheese from Hannah to eat now)');
check('msg1 carries the tool_call', Array.isArray(msgs[1].tool_calls) && msgs[1].tool_calls[0].function.name === 'pay_with_item');
check('msg1 NOT time-prefixed', !msgs[1].content.startsWith('['));
// tool result: untouched.
check('msg2 is tool', msgs[2].role === 'tool' && msgs[2].tool_call_id === 't1' && msgs[2].content === '[ok]');
// 2nd perception: gap marker (~3h) + [5m].
check('msg3 is user', msgs[3].role === 'user');
check('msg3 has the gap marker', msgs[3].content.startsWith('--- gap: 3h ---\n[5 minutes ago]\n# Your turn'));

// A perception with no sent_at -> no prefix (graceful).
const noTime = buildToolUseMessages([{ from_agent: 'salem-engine', message: '# Your turn\nno time' }], NPC);
check('no sent_at -> raw content, no prefix', noTime[0].content === '# Your turn\nno time');

// ZBBS-HOME-436: a REJECTED quote-take replays its "bought" paraphrase with
// the [error] tool result immediately adjacent (paired by tool_call_id in
// stored row order), so the false-success window the wording opens is closed
// in the same breath. This pins the invariant the paraphrase relies on.
const rejected = buildToolUseMessages([
    { from_agent: 'salem-engine', message: '# Your turn', sent_at: agoISO(10) },
    { from_agent: NPC, message: '', tool_calls: [{ id: 't9', name: 'pay_with_item', input: { item: 'Meat', seller: 'John Ellis', quote_id: 1, consume_now: true } }], sent_at: agoISO(9) },
    { from_agent: 'salem-engine', message: '[error: quote expired]', tool_call_id: 't9', sent_at: agoISO(9) },
], NPC);
check('rejected quote-take: bought paraphrase', rejected[1].content === '(I bought Meat from John Ellis and ate it on the spot)');
check('rejected quote-take: [error] result directly follows', rejected[2].role === 'tool' && rejected[2].tool_call_id === 't9' && rejected[2].content === '[error: quote expired]');

// LLM-237: a parallel-tool-call turn (Llama's normal speak+done shape) is
// split at replay time into one assistant message per call, each directly
// followed by its id-matched tool result — vLLM-based OpenRouter upstreams
// 400 any request whose history has an assistant message with >1 tool_calls.
const parallel = buildToolUseMessages([
    { from_agent: 'salem-engine', message: '# Your turn', sent_at: agoISO(10) },
    { from_agent: NPC, message: '', tool_calls: [
        { id: 'p1', name: 'speak', input: { text: 'Fresh bread today!' } },
        { id: 'p2', name: 'done', input: {} },
    ], sent_at: agoISO(9) },
    { from_agent: 'salem-engine', message: '[ok] You said: "Fresh bread today!"', tool_call_id: 'p1', sent_at: agoISO(9) },
    { from_agent: 'salem-engine', message: '[done]', tool_call_id: 'p2', sent_at: agoISO(9) },
    { from_agent: 'salem-engine', message: '# Your turn again', sent_at: agoISO(8) },
], NPC);
check('parallel: 6 messages (1 user + 2 assistant/tool pairs + 1 user)', parallel.length === 6);
check('parallel: piece 1 is assistant with ONLY the speak call',
    parallel[1].role === 'assistant' && parallel[1].tool_calls.length === 1 && parallel[1].tool_calls[0].function.name === 'speak');
check('parallel: piece 1 carries the speak paraphrase', parallel[1].content === '(I said aloud: "Fresh bread today!")');
check('parallel: speak result directly follows its piece',
    parallel[2].role === 'tool' && parallel[2].tool_call_id === 'p1');
check('parallel: piece 2 is assistant with ONLY the done call',
    parallel[3].role === 'assistant' && parallel[3].tool_calls.length === 1 && parallel[3].tool_calls[0].function.name === 'done');
check('parallel: done has no paraphrase (empty content)', parallel[3].content === '');
check('parallel: done result directly follows its piece',
    parallel[4].role === 'tool' && parallel[4].tool_call_id === 'p2' && parallel[4].content === '[done]');
check('parallel: trailing perception intact', parallel[5].role === 'user' && parallel[5].content.includes('# Your turn again'));

// Model-authored text on a parallel turn stays on the FIRST piece only —
// later pieces get their own paraphrase (or empty), never a duplicate.
const parallelText = buildToolUseMessages([
    { from_agent: 'salem-engine', message: '# Your turn', sent_at: agoISO(10) },
    { from_agent: NPC, message: 'Let me think on that.', tool_calls: [
        { id: 'q1', name: 'speak', input: { text: 'Aye.' } },
        { id: 'q2', name: 'done', input: {} },
    ], sent_at: agoISO(9) },
    { from_agent: 'salem-engine', message: '[ok]', tool_call_id: 'q1', sent_at: agoISO(9) },
    { from_agent: 'salem-engine', message: '[done]', tool_call_id: 'q2', sent_at: agoISO(9) },
], NPC);
check('parallel+text: model text on first piece', parallelText[1].content === 'Let me think on that.');
check('parallel+text: second piece empty, not duplicated', parallelText[3].content === '');

// A missing tool result (generation raced the engine's result writes) gets a
// synthesized neutral "[ok]" so no split piece carries a dangling tool_call.
const missingResult = buildToolUseMessages([
    { from_agent: 'salem-engine', message: '# Your turn', sent_at: agoISO(10) },
    { from_agent: NPC, message: '', tool_calls: [
        { id: 'm1', name: 'speak', input: { text: 'Hm.' } },
        { id: 'm2', name: 'done', input: {} },
    ], sent_at: agoISO(9) },
    { from_agent: 'salem-engine', message: '[ok]', tool_call_id: 'm1', sent_at: agoISO(9) },
], NPC);
check('missing result: still fully paired (5 messages)', missingResult.length === 5);
check('missing result: synthesized [ok] for the unanswered call',
    missingResult[4].role === 'tool' && missingResult[4].tool_call_id === 'm2' && missingResult[4].content === '[ok]');

// Single-call turns are untouched by the split (shape identical to before).
check('single-call turn untouched', msgs[1].tool_calls.length === 1 && msgs[2].role === 'tool');

if (failed > 0) {
    console.log('FAILED ' + failed + ' / ' + (passed + failed));
    failures.forEach(f => console.log(f));
    process.exit(1);
}
console.log('PASS ' + passed + ' / ' + (passed + failed));
process.exit(0);
