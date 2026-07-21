#!/usr/bin/env node
// Standalone test for stabilizeHistoryWindow (virtual-agent.js, LLM-501) —
// the quantizer that stops a cap-bound history window's head from sliding
// one row per turn (a full provider prefix-cache miss every tick). Same
// require-stub preamble as test-build-tool-use-messages.js so the heavy
// module graph never loads.
//
// Run: node scripts/test-stabilize-history-window.js   (exits 0 pass / 1 fail)

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

const { stabilizeHistoryWindow } = require('../src/services/virtual-agent');

let passed = 0, failed = 0;
const failures = [];
function check(label, cond) {
    if (cond) { passed++; } else { failed++; failures.push('  FAIL [' + label + ']'); }
}

const GRID_MS = 15 * 60 * 1000;
const CAP = 50;

// Build n rows spaced stepMs apart, the NEWEST at endMs and the oldest
// earliest — mirroring loadDirectChatHistory's oldest-first return order.
function rows(n, endMs, stepMs) {
    const out = [];
    for (let i = n - 1; i >= 0; i--) {
        out.push({ sent_at: new Date(endMs - i * stepMs).toISOString(), message: 'm' + i });
    }
    return out;
}

// Fix an absolute anchor mid-band so grid math is deterministic in the test:
// 2026-07-21T12:07:30Z is 7.5 minutes past a 15-minute grid line.
const ANCHOR = Date.parse('2026-07-21T12:07:30Z');

// Under the cap: untouched (the hour-quantized SQL time cutoff governs).
const under = rows(30, ANCHOR, 60000);
check('under cap -> untouched', stabilizeHistoryWindow(under, CAP) === under);

// At the cap: head trimmed to the first row at/after the grid line above the
// oldest fetched row.
const atCap = rows(CAP, ANCHOR, 60000); // oldest at 11:18:30 -> grid line 11:30:00
const trimmed = stabilizeHistoryWindow(atCap, CAP);
const gridLine = Math.ceil((ANCHOR - (CAP - 1) * 60000) / GRID_MS) * GRID_MS;
check('at cap -> head trimmed to grid line',
    trimmed.length > 0 && new Date(trimmed[0].sent_at).getTime() >= gridLine);
check('at cap -> rows before grid line dropped',
    atCap.filter(r => new Date(r.sent_at).getTime() >= gridLine).length === trimmed.length);
check('at cap -> tail preserved in order',
    trimmed[trimmed.length - 1] === atCap[atCap.length - 1]);

// STABILITY: one turn later (one new row appended, oldest fell off the
// fetch) the retained head must be IDENTICAL as long as the oldest fetched
// row stays inside the same grid band — that identity is the whole point.
const nextTurn = rows(CAP, ANCHOR + 60000, 60000); // slid forward one row
const trimmedNext = stabilizeHistoryWindow(nextTurn, CAP);
check('next turn, same band -> identical head timestamp',
    trimmedNext[0].sent_at === trimmed[0].sent_at);

// Boundary semantics: an oldest row exactly ON a grid line closes its band —
// the grid line IS that timestamp, so nothing is trimmed on this turn...
const BOUNDARY = Math.ceil(ANCHOR / GRID_MS) * GRID_MS;
const onBoundary = rows(CAP, BOUNDARY + (CAP - 1) * 60000, 60000); // oldest exactly at BOUNDARY
check('oldest exactly on grid line -> retained from that row',
    stabilizeHistoryWindow(onBoundary, CAP)[0].sent_at === onBoundary[0].sent_at);
// ...and once the oldest crosses into the next band, the head jumps to the
// next line — exactly one jump, after which it is stable again.
const crossed = rows(CAP, BOUNDARY + CAP * 60000, 60000); // oldest at BOUNDARY+1min
const crossedTrimmed = stabilizeHistoryWindow(crossed, CAP);
const nextLine = BOUNDARY + GRID_MS;
check('oldest past the line -> head jumps to the next grid line',
    new Date(crossedTrimmed[0].sent_at).getTime() >= nextLine);
const crossedAgain = rows(CAP, BOUNDARY + (CAP + 5) * 60000, 60000); // 5 turns later, same band
check('subsequent turns in the new band -> head identical',
    stabilizeHistoryWindow(crossedAgain, CAP)[0].sent_at === crossedTrimmed[0].sent_at);

// Quality floor: when every fetched row sits before the grid line (a dense
// burst just under a boundary), the floor binds — exactly the newest
// MIN_KEEP rows are returned, deliberately unaligned to the grid (cache
// stability abandoned for context; see the implementation comment).
const dense = rows(CAP, BOUNDARY - 60000, 5000); // 50 rows, all inside one band, none at/after its line
const denseTrimmed = stabilizeHistoryWindow(dense, CAP);
check('floor binds -> exactly the newest 25 rows', denseTrimmed.length === 25);
check('floor binds -> retained head is the 25th-newest row',
    denseTrimmed[0] === dense[CAP - 25]);

// Graceful on rows without sent_at at the head.
const noTime = [{ message: 'x' }].concat(rows(CAP - 1, ANCHOR, 60000));
check('missing head sent_at -> untouched', stabilizeHistoryWindow(noTime, CAP) === noTime);

// Empty / non-array input passes through.
check('empty -> untouched', Array.isArray(stabilizeHistoryWindow([], CAP)) );

if (failed > 0) {
    console.log('FAILED ' + failed + ' / ' + (passed + failed));
    failures.forEach(f => console.log(f));
    process.exit(1);
}
console.log('PASS ' + passed + ' / ' + (passed + failed));
process.exit(0);
