#!/usr/bin/env node
// Standalone test for buildCurrentTurnContext (virtual-agent.js, LLM-501) —
// the assembler for the volatile context attached to the model's current turn
// on the sim tool-use path: the Impressions block (moved out of the sim system
// prompt, where a co-location flip was a full-request cache miss) followed by
// the engine's ephemeral_context. Same require-stub preamble as the sibling
// scripts so the heavy module graph never loads.
//
// Run: node scripts/test-build-current-turn-context.js   (exits 0 pass / 1 fail)

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

const { buildCurrentTurnContext } = require('../src/services/virtual-agent');

let passed = 0, failed = 0;
const failures = [];
function check(label, cond) {
    if (cond) { passed++; } else { failed++; failures.push('  FAIL [' + label + ']'); }
}

const PEOPLE = '## Your impressions of Hannah Boggs\n\nFair dealer, pays promptly.';
const EPHEMERAL = '## You\nCoins in your purse: 3.\n\nWeigh what is in front of you.';

// Sim tick with both: Impressions block leads, ephemeral (with its closing
// triage coda) stays last.
const both = buildCurrentTurnContext(true, PEOPLE, EPHEMERAL);
check('impressions wrapped in the Impressions block', both.includes('<Impressions purpose="private-relationship-notes">') && both.includes(PEOPLE));
check('ephemeral body present', both.includes(EPHEMERAL));
check('impressions precede the ephemeral body', both.indexOf('<Impressions') < both.indexOf('## You'));
check('ephemeral is last', both.endsWith(EPHEMERAL));

// Empty peopleContext: NO empty Impressions wrapper — the payload is exactly
// the ephemeral body.
check('empty impressions -> ephemeral only', buildCurrentTurnContext(true, '', EPHEMERAL) === EPHEMERAL);
check('whitespace impressions -> ephemeral only', buildCurrentTurnContext(true, '  \n ', EPHEMERAL) === EPHEMERAL);

// Impressions with no ephemeral (tool-result follow-up with no perception).
const impOnly = buildCurrentTurnContext(true, PEOPLE, null);
check('impressions alone -> just the block', impOnly.startsWith('<Impressions') && impOnly.includes(PEOPLE));

// Non-sim callers contribute no Impressions here (theirs stays in the system
// prompt) — only the ephemeral passes through.
check('non-sim -> no impressions block', buildCurrentTurnContext(false, PEOPLE, EPHEMERAL) === EPHEMERAL);

// Nothing to attach -> empty string (caller skips the attachment).
check('nothing -> empty', buildCurrentTurnContext(true, '', null) === '');

if (failed > 0) {
    console.log('FAILED ' + failed + ' / ' + (passed + failed));
    failures.forEach(f => console.log(f));
    process.exit(1);
}
console.log('PASS ' + passed + ' / ' + (passed + failed));
process.exit(0);
