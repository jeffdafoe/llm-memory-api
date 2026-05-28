#!/usr/bin/env node
// Standalone unit test for extractCoLocatedNames (virtual-agent.js).
//
// Pins the parser to v2 Salem engine perception format:
//   ## Around you
//   inside: <Tavern> [tavern]
//   huddle: <id> with Name1, Name2, the keeper, a stranger
//   atmosphere: ...
//
// And verifies the failure modes that bit stateful Salem NPCs before the
// v2 rewrite of this function:
//   - v1 "Here:\n  Name" format produces empty (v1 is being deleted).
//   - Acquaintance descriptors ("the X", "a stranger") are filtered so
//     they don't reach personContextSlug and produce bogus lookups.
//   - Solo / not-in-huddle states return empty.
//
// Run: node scripts/test-extract-co-located-names.js
// Exits 0 on pass, 1 on any failure.
//
// virtual-agent.js requires a few modules at load that touch DB/Express
// internals. None are exercised by extractCoLocatedNames — but the
// top-level require could touch the environment. Stub the heavy
// dependencies before loading the module to keep the test pure.

const Module = require('module');
const origRequire = Module.prototype.require;

// Stub ../db, ../models/*, anything that would open a pool. The parser
// is pure; we only need the exported function reference.
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

const { extractCoLocatedNames } = require('../src/services/virtual-agent');

let passed = 0;
let failed = 0;
const failures = [];

function assertEqual(label, got, want) {
    const gotJson = JSON.stringify(got);
    const wantJson = JSON.stringify(want);
    if (gotJson === wantJson) {
        passed++;
    } else {
        failed++;
        failures.push('  FAIL [' + label + ']: got ' + gotJson + ', want ' + wantJson);
    }
}

// ---------- v2 happy paths (post-WORK-348: no debug ids in lines) ----------

assertEqual(
    'two acquainted peers',
    extractCoLocatedNames(
        '## Around you\n' +
        'inside: Tavern\n' +
        'huddle: with Prudence Ward, Josiah Thorne\n' +
        'atmosphere: quiet\n'
    ),
    ['Prudence Ward', 'Josiah Thorne']
);

assertEqual(
    'single acquainted peer',
    extractCoLocatedNames(
        '## Around you\n' +
        'inside: Apothecary\n' +
        'huddle: with Prudence Ward\n'
    ),
    ['Prudence Ward']
);

assertEqual(
    'name with apostrophe + hyphen survives intact',
    extractCoLocatedNames('huddle: with Mary O\'Brien, Jean-Luc Picard\n'),
    ["Mary O'Brien", 'Jean-Luc Picard']
);

// ---------- v2 acquaintance-descriptor filtering ----------

assertEqual(
    'mixed acquainted + role + stranger drops descriptors',
    extractCoLocatedNames('huddle: with Prudence Ward, the keeper, a stranger\n'),
    ['Prudence Ward']
);

assertEqual(
    'all unacquainted yields empty',
    extractCoLocatedNames('huddle: with the keeper, the blacksmith, a stranger\n'),
    []
);

assertEqual(
    'capitalized "The X" player name is NOT filtered',
    extractCoLocatedNames('huddle: with The Mountain, Sansa Stark\n'),
    ['The Mountain', 'Sansa Stark']
);

// ---------- v2 solo / no-huddle states ----------

assertEqual(
    'alone in huddle (you are the only member)',
    extractCoLocatedNames(
        '## Around you\n' +
        'inside: Tavern\n' +
        'huddle: (you are the only member)\n'
    ),
    []
);

assertEqual(
    'not in a huddle',
    extractCoLocatedNames(
        '## Around you\n' +
        'inside: outdoors\n' +
        'huddle: not in a huddle\n'
    ),
    []
);

// Regression guard: the pre-WORK-348 format with a leading huddle id
// MUST NOT match — the new parser is anchored on `huddle: with` exactly.
// If the engine ever re-introduced a huddle id between the label and
// "with", this would silently break, and this test pins that contract.
assertEqual(
    'pre-WORK-348 "huddle: h1 with ..." does NOT match (id-anchored parser is gone)',
    extractCoLocatedNames('huddle: h1 with Prudence Ward, Josiah Thorne\n'),
    []
);

assertEqual(
    'no Around-you block at all',
    extractCoLocatedNames('## Orders you\'re waiting on\n- order id 7\n'),
    []
);

// ---------- defensive ----------

assertEqual('null input', extractCoLocatedNames(null), []);
assertEqual('undefined input', extractCoLocatedNames(undefined), []);
assertEqual('empty string', extractCoLocatedNames(''), []);

// ---------- v1 format (deliberately UNSUPPORTED) ----------
// v1 emitted "Here:\n  Name (inside)" — that engine is being deleted
// post-v2-go-live (project_salem_v1_deletion). The v2 parser should NOT
// match v1's shape; this pins the deliberate scope.

assertEqual(
    'v1 "Here:" format returns empty (v1 unsupported)',
    extractCoLocatedNames('Here:\n  Prudence Ward (inside)\n'),
    []
);

// ---------- Report ----------

if (failed > 0) {
    console.log('FAILED ' + failed + ' / ' + (passed + failed));
    failures.forEach(function (f) { console.log(f); });
    process.exit(1);
}
console.log('PASS ' + passed + ' / ' + (passed + failed));
process.exit(0);
