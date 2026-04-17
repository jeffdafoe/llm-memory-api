#!/usr/bin/env node
// Standalone unit test for the MCP tool-argument validator.
//
// Runs a fixed set of adversarial inputs through validateToolArgs and checks
// that each throws with statusCode 400 (bad input) rather than a 500 crash.
// No server, no DB, no network — pure function-under-test verification.
//
// Run: node scripts/test-mcp-input-validation.js
// Exits 0 on pass, 1 on any failure.

const { validateToolArgs, TOOLS } = require('../src/routes/mcp');

let passed = 0;
let failed = 0;
const failures = [];

// assertValid: call should NOT throw
function assertValid(label, toolName, args) {
    try {
        validateToolArgs(toolName, args);
        passed++;
    } catch (err) {
        failed++;
        failures.push(`  FAIL [${label}] ${toolName}: unexpectedly threw: ${err.message}`);
    }
}

// assertRejects: call should throw a 400 (never reach 500)
function assertRejects(label, toolName, args) {
    try {
        validateToolArgs(toolName, args);
        failed++;
        failures.push(`  FAIL [${label}] ${toolName}: should have thrown but did not`);
    } catch (err) {
        if (err.statusCode === 400) {
            passed++;
        } else {
            failed++;
            failures.push(`  FAIL [${label}] ${toolName}: wrong statusCode ${err.statusCode}: ${err.message}`);
        }
    }
}

// ---------- Envelope checks (same for every tool) ----------

const sampleTool = 'search';
assertRejects('args is null', sampleTool, null);
assertRejects('args is array', sampleTool, []);
assertRejects('args is string', sampleTool, 'hello');
assertRejects('args is number', sampleTool, 42);

// ---------- Per-tool: required fields missing ----------

for (const tool of TOOLS) {
    const required = (tool.inputSchema && tool.inputSchema.required) || [];
    if (required.length === 0) continue; // tools with no required fields — skip

    // Missing entirely
    assertRejects(`missing required`, tool.name, {});

    // Each required field missing in turn (others filled with plausible values)
    const template = buildTemplateArgs(tool);
    for (const fieldName of required) {
        const args = { ...template };
        delete args[fieldName];
        assertRejects(`missing "${fieldName}"`, tool.name, args);

        // null and undefined should be treated as missing
        const argsWithNull = { ...template, [fieldName]: null };
        assertRejects(`null "${fieldName}"`, tool.name, argsWithNull);

        const argsWithUndef = { ...template, [fieldName]: undefined };
        assertRejects(`undefined "${fieldName}"`, tool.name, argsWithUndef);
    }
}

// ---------- Per-tool: required string fields must be non-empty ----------

for (const tool of TOOLS) {
    const required = (tool.inputSchema && tool.inputSchema.required) || [];
    const props = (tool.inputSchema && tool.inputSchema.properties) || {};
    const template = buildTemplateArgs(tool);

    for (const fieldName of required) {
        if (!props[fieldName] || props[fieldName].type !== 'string') continue;

        assertRejects(`empty string "${fieldName}"`, tool.name, { ...template, [fieldName]: '' });
        assertRejects(`whitespace "${fieldName}"`, tool.name, { ...template, [fieldName]: '   ' });
    }
}

// ---------- Per-tool: type mismatches ----------

for (const tool of TOOLS) {
    const props = (tool.inputSchema && tool.inputSchema.properties) || {};
    const template = buildTemplateArgs(tool);

    for (const [fieldName, fieldSchema] of Object.entries(props)) {
        const wrongValues = wrongTypeValues(fieldSchema.type);
        for (const wrongValue of wrongValues) {
            assertRejects(
                `${fieldSchema.type} expected, got ${describe(wrongValue)} at "${fieldName}"`,
                tool.name,
                { ...template, [fieldName]: wrongValue }
            );
        }
    }
}

// ---------- Per-tool: array items type enforcement ----------

// chat_ack.ids is array of numbers — string items must reject
assertRejects('chat_ack ids as strings', 'chat_ack', { ids: ['not-a-number'] });
assertRejects('chat_ack ids mixed', 'chat_ack', { ids: [1, 'two', 3] });
assertValid('chat_ack ids all numbers', 'chat_ack', { ids: [1, 2, 3] });

// mail_ack.ids is array of strings — number items must reject
assertRejects('mail_ack ids as numbers', 'mail_ack', { ids: [1, 2] });
assertValid('mail_ack ids all strings', 'mail_ack', { ids: ['uuid-a', 'uuid-b'] });

// update_expertise.expertise is array of strings
assertRejects('update_expertise numbers', 'update_expertise', { expertise: [1, 2] });
assertValid('update_expertise strings', 'update_expertise', { expertise: ['codebase', 'ops'] });

// ---------- Happy path: valid inputs pass ----------

assertValid('search minimal', 'search', { query: 'hello world' });
assertValid('search with namespace + limit', 'search', { query: 'hello', namespace: 'home', limit: 10 });
assertValid('save_note minimal', 'save_note', { title: 'Test', content: 'Body' });
assertValid('list_notes empty args', 'list_notes', {});
assertValid('read_note minimal', 'read_note', { slug: 'some/slug' });
assertValid('edit_note with boolean', 'edit_note', { slug: 'x', old_string: 'a', new_string: 'b', replace_all: true });
assertValid('discussion_vote_cast with reason', 'discussion_vote_cast', { vote_id: 1, choice: 2, reason: 'explains' });
assertValid('activity_start no args', 'activity_start', {});

// ---------- Infinity / NaN must reject on number fields ----------

assertRejects('search limit NaN', 'search', { query: 'hi', limit: NaN });
assertRejects('search limit Infinity', 'search', { query: 'hi', limit: Infinity });
assertRejects('search limit string', 'search', { query: 'hi', limit: '5' });

// ---------- Unknown tool ----------

assertRejects('unknown tool', 'nonexistent_tool', {});

// ---------- Envelope: null / undefined behavior ----------
// undefined should normalize to {} for zero-arg tools; null must stay rejected.

assertValid('undefined args on zero-arg tool', 'activity_start', undefined);
assertRejects('null args on zero-arg tool', 'activity_start', null);
assertRejects('null args on required-field tool', 'search', null);

// ---------- Prototype check: non-plain objects rejected ----------

function makeWithCustomProto() {
    class Weird {}
    const o = new Weird();
    o.query = 'hello';
    return o;
}
assertRejects('non-plain object rejected', 'search', makeWithCustomProto());

const nullProtoArgs = Object.create(null);
nullProtoArgs.query = 'hello';
assertValid('null-prototype object accepted', 'search', nullProtoArgs);

// ---------- Own-property vs prototype-chain ----------
// A required field inherited from a prototype must NOT satisfy the check.

const parentObj = { query: 'inherited' };
const childArgs = Object.create(parentObj);
// childArgs has `query` only via prototype chain — should reject.
assertRejects('required field only on prototype', 'search', childArgs);

// ---------- Explicit null on optional field = type error ----------

assertRejects('optional string field null', 'search', { query: 'hi', namespace: null });
assertRejects('optional number field null', 'search', { query: 'hi', limit: null });
assertRejects('optional boolean field null', 'edit_note', { slug: 's', old_string: 'a', new_string: 'b', replace_all: null });

// ---------- Array length cap ----------

const bigArray = new Array(1001).fill('x');
assertRejects('mail_ack ids > 1000', 'mail_ack', { ids: bigArray });

const exactCap = new Array(1000).fill('x');
assertValid('mail_ack ids == 1000 (cap)', 'mail_ack', { ids: exactCap });

// ---------- Helpers ----------

function buildTemplateArgs(tool) {
    const args = {};
    const props = (tool.inputSchema && tool.inputSchema.properties) || {};
    for (const [name, schema] of Object.entries(props)) {
        args[name] = sampleForType(schema);
    }
    return args;
}

function sampleForType(fieldSchema) {
    switch (fieldSchema.type) {
        case 'string': return 'sample';
        case 'number': return 1;
        case 'boolean': return true;
        case 'array':
            if (fieldSchema.items && fieldSchema.items.type === 'number') return [1];
            return ['item'];
        case 'object': return {};
        default: return 'sample';
    }
}

function wrongTypeValues(type) {
    switch (type) {
        case 'string': return [42, true, [], {}];
        case 'number': return ['42', true, [], {}];
        case 'boolean': return ['true', 1, [], {}];
        case 'array': return ['not-an-array', 42, {}];
        case 'object': return ['not-an-object', 42, []];
        default: return [];
    }
}

function describe(v) {
    if (v === null) return 'null';
    if (Array.isArray(v)) return 'array';
    return typeof v;
}

// ---------- Report ----------

console.log(`\nRan ${passed + failed} cases: ${passed} passed, ${failed} failed`);
if (failed > 0) {
    console.log('\nFailures:');
    for (const line of failures) console.log(line);
    process.exit(1);
}
console.log('All validator cases passed.');
process.exit(0);
