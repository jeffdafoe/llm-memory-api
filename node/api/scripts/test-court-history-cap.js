#!/usr/bin/env node
// Standalone test for the court history cap (virtual-agent.js, LLM-819). A
// court hearing past 50 rows used to replay only a slid window, and the head
// trim then removed every leading tool row (the only user rows in a hearing
// are the opener and the nudges), leaving the magistrate with nothing but the
// "Continue." seed. Same require-stub preamble as the sibling scripts so the
// heavy module graph never loads.
//
// Run: node scripts/test-court-history-cap.js   (exits 0 pass / 1 fail)

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

const {
    historyCapFor, stabilizeHistoryWindow, buildToolUseMessages, trimHeadToUserMessage,
} = require('../src/services/virtual-agent');

let passed = 0, failed = 0;
const failures = [];
function check(label, cond) {
    if (cond) { passed++; } else { failed++; failures.push('  FAIL [' + label + ']'); }
}

const MAGISTRATE = 'salem-magistrate';
const OPENER = 'The court is in session. Hear the matter set out before you and give your ruling.';

// A hearing as loadDirectChatHistory returns it (oldest first): the opener,
// then `rounds` tool rounds (one assistant tool call + its result), all inside
// one minute — the shape of a real hearing.
function hearingRows(rounds) {
    const start = Date.parse('2026-10-08T16:00:05Z');
    let t = start;
    function at() { t += 500; return new Date(t).toISOString(); }
    const out = [{ from_agent: 'salem-engine', to_agent: MAGISTRATE, message: OPENER, sent_at: at() }];
    for (let i = 1; i <= rounds; i++) {
        out.push({
            from_agent: MAGISTRATE, to_agent: 'salem-engine', message: '', sent_at: at(),
            tool_calls: [{ id: 'call_' + i, name: 'read_record', input: { n: i } }],
        });
        out.push({
            from_agent: 'salem-engine', to_agent: MAGISTRATE, message: 'record ' + i, sent_at: at(),
            tool_call_id: 'call_' + i,
        });
    }
    return out;
}

// The sim tool-use replay path for one fetch: the window trim at the cap,
// then the replay build and the head trim.
function replay(fetchedRows, cap) {
    return trimHeadToUserMessage(buildToolUseMessages(stabilizeHistoryWindow(fetchedRows, cap), MAGISTRATE));
}

check('magistrate gets the court cap', historyCapFor(MAGISTRATE) === 500);
check('other agents keep the 50-row cap', historyCapFor('zbbs-prudence-ward') === 50 && historyCapFor('salem-vendor') === 50);
check('24 rounds of 19 parallel calls fit the court cap', 1 + 24 * (1 + 19) <= historyCapFor(MAGISTRATE));

const all = hearingRows(32); // 65 rows: past the old 50-row cap

// The defect: a 50-row fetch of this hearing, through the 50-row window,
// leaves no user turn, so the replay is empty (handleDirectChat then seeds
// "Continue.").
check('old cap: replay is empty', replay(all.slice(-50), 50).length === 0);

// The fix: the court fetch returns the whole hearing and the window does not
// trim it, so the replay starts at the opener and keeps every round.
const fixed = replay(all, historyCapFor(MAGISTRATE));
check('court cap: replay starts with the opener', fixed.length > 0 && fixed[0].role === 'user' && fixed[0].content.includes(OPENER));
check('court cap: every tool result is kept', fixed.filter(m => m.role === 'tool').length === 32);

// Byte-stable from one round to the next: round N's replay is a prefix of
// round N+1's, so the LLM-816 history cache keeps hitting past 50 rows.
const roundN = replay(hearingRows(30), historyCapFor(MAGISTRATE));
const roundNext = replay(hearingRows(31), historyCapFor(MAGISTRATE));
check('court cap: round N replay is a prefix of round N+1',
    JSON.stringify(roundNext.slice(0, roundN.length)) === JSON.stringify(roundN));

if (failed > 0) {
    console.log('FAILED ' + failed + ' / ' + (passed + failed));
    failures.forEach(f => console.log(f));
    process.exit(1);
}
console.log('PASS ' + passed + ' / ' + (passed + failed));
process.exit(0);
