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
check('msg0 prefixed with [3h]', msgs[0].content.startsWith('[3h]\n# Your turn'));
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
check('msg3 has the gap marker', msgs[3].content.startsWith('--- gap: 3h ---\n[5m]\n# Your turn'));

// A perception with no sent_at -> no prefix (graceful).
const noTime = buildToolUseMessages([{ from_agent: 'salem-engine', message: '# Your turn\nno time' }], NPC);
check('no sent_at -> raw content, no prefix', noTime[0].content === '# Your turn\nno time');

if (failed > 0) {
    console.log('FAILED ' + failed + ' / ' + (passed + failed));
    failures.forEach(f => console.log(f));
    process.exit(1);
}
console.log('PASS ' + passed + ' / ' + (passed + failed));
process.exit(0);
