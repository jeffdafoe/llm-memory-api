// Writes goldens.json beside this file: the Node server's MCP tool list, its
// permission map, and what validateToolArgs says for a corpus of arguments,
// so the Go port's unit tests compare against Node itself. Run from the
// repository root, after `npm ci` in node/api:
//
//     node go/server/internal/mcp/testdata/goldens.js
//
// TOOLS and validateToolArgs are exported by routes/mcp.js. TOOL_PERMISSIONS
// is not, so its source is read from the module text and evaluated here.
// Loading the module opens no connection: the pg pool connects on first use.

const fs = require('fs');
const path = require('path');

const modulePath = path.resolve(__dirname, '../../../../../node/api/src/routes/mcp.js');
const mcp = require(modulePath);
const source = fs.readFileSync(modulePath, 'utf8');

const permMatch = source.match(/\nconst TOOL_PERMISSIONS = (\{[\s\S]*?\n\});\n/);
if (!permMatch) {
    throw new Error('TOOL_PERMISSIONS not found in routes/mcp.js');
}
const toolPermissions = new Function('return ' + permMatch[1] + ';')();

// Each case is the tool name and the arguments as JSON text, or null for
// arguments that were not sent (undefined).
const cases = [];
function add(tool, argsText) {
    cases.push({ tool, args: argsText });
}

function sampleForType(fieldSchema) {
    switch (fieldSchema.type) {
        case 'string': return 'sample';
        case 'number': return 1;
        case 'boolean': return true;
        case 'array':
            if (fieldSchema.items && fieldSchema.items.type === 'number') {
                return [1];
            }
            return ['item'];
        case 'object': return {};
        default: return 'sample';
    }
}

function template(tool) {
    const args = {};
    const props = tool.inputSchema.properties || {};
    for (const [name, schema] of Object.entries(props)) {
        args[name] = sampleForType(schema);
    }
    return args;
}

// JSON texts of the wrong JSON types per declared type, plus the values JSON
// can carry that JavaScript reads specially (1e400 is Infinity, -0).
const wrongValues = {
    string: ['42', 'true', '[]', '{}', 'null', '0', '""'],
    number: ['"42"', 'true', '[]', '{}', 'null', '1e400', '-1e400', '-0', '0', '1.5', '1e-400', '9007199254740993'],
    boolean: ['"true"', '1', '[]', '{}', 'null', 'false', '0'],
    array: ['"not-an-array"', '42', '{}', 'null', '[]', '[null]', '[1, "a"]', '[[]]', '[{}]', '["a", 1e400]', '[1e400]', '[-0]'],
    object: ['"x"', '42', '[]', 'null', '{}'],
};

// Envelopes.
for (const tool of mcp.TOOLS) {
    add(tool.name, null);
    for (const text of ['null', '[]', '"hello"', '42', 'true', 'false', '{}', '{"__proto__": {"query": "x"}}', '{"constructor": 1}']) {
        add(tool.name, text);
    }
}
add('nonexistent_tool', '{}');
add('nonexistent_tool', null);
add('__proto__', '{}');
add('constructor', '{}');
add('toString', '{}');

for (const tool of mcp.TOOLS) {
    const required = tool.inputSchema.required || [];
    const props = tool.inputSchema.properties || {};
    const base = template(tool);
    add(tool.name, JSON.stringify(base));
    for (const field of required) {
        const missing = { ...base };
        delete missing[field];
        add(tool.name, JSON.stringify(missing));
        if (props[field] && props[field].type === 'string') {
            for (const blank of ['""', '"   "', '"\\t\\n"', '"\\u00a0"', '"\\ufeff"', '"\\u2028"', '"\\u200b"', '" x "']) {
                add(tool.name, JSON.stringify(base).replace(/}$/, '') .replace(new RegExp('"' + field + '":"sample"'), '"' + field + '":' + blank) + '}');
            }
        }
    }
    for (const [field, schema] of Object.entries(props)) {
        for (const wrong of wrongValues[schema.type] || []) {
            const others = { ...base };
            delete others[field];
            const head = JSON.stringify(others).slice(0, -1);
            const sep = head === '{' ? '' : ',';
            add(tool.name, head + sep + JSON.stringify(field) + ':' + wrong + '}');
        }
        // An inherited-looking key is still an own property after JSON.parse.
        add(tool.name, '{"__proto__":' + JSON.stringify(base) + '}');
    }
}

// Array length cap and item checks.
add('mail_ack', JSON.stringify({ ids: new Array(1001).fill('x') }));
add('mail_ack', JSON.stringify({ ids: new Array(1000).fill('x') }));
add('chat_ack', JSON.stringify({ ids: new Array(1001).fill(1) }));
add('chat_ack', '{"ids": ["not-a-number"]}');
add('chat_ack', '{"ids": [1, "two", 3]}');
add('chat_ack', '{"ids": [1, 2, 3]}');
add('chat_ack', '{"ids": [1e400]}');
add('mail_ack', '{"ids": [1, 2]}');
add('update_expertise', '{"expertise": [1, 2]}');
add('update_expertise', '{"expertise": ["codebase", "ops"]}');
add('move_folder', '{"from_prefix": "a/", "to_prefix": "", "overwrite_slugs": [true]}');
add('move_folder', '{"from_prefix": "a/", "to_prefix": " "}');
add('move_folder', '{"from_prefix": "a/", "to_prefix": "b/", "overwrite_slugs": []}');
add('search', '{"query": "hi", "limit": 5, "extra": {"nested": [1, 2]}}');
add('search', '{"query": "hi", "query": ""}');
add('search', '{"query": "", "query": "hi"}');
add('search', '{"query": "hi", "limit": 1, "limit": "x"}');
add('discussion_vote_cast', '{"vote_id": 1, "choice": 2, "reason": "explains"}');
add('activity_start', '{"anything": null}');
add('read_instructions', '{"x": 1e400}');

const validation = cases.map((c) => {
    const args = c.args === null ? undefined : JSON.parse(c.args);
    try {
        mcp.validateToolArgs(c.tool, args);
        return { tool: c.tool, args: c.args, ok: true };
    } catch (err) {
        return { tool: c.tool, args: c.args, ok: false, status: err.statusCode, error: err.message };
    }
});

// The SDK's Content-Type check, over header values as Node holds them (one
// character per byte).
const { isJsonContentType } = require(require.resolve('@modelcontextprotocol/server',
    { paths: [path.resolve(__dirname, '../../../../../node/api')] }));
const contentTypes = [
    'application/json', 'APPLICATION/JSON', 'Application/Json', 'application/json; charset=utf-8',
    'application/json;charset=utf-8', 'application/json ; charset=utf-8', 'application/json;', 'application/json; ',
    'application/json; profile="a,b"', 'application/json; profile="a\\"b"', 'application/json; a=b, c',
    'application/json, text/plain', 'application/json,text/plain', 'application/json; a="unterminated',
    'application/json; a', 'application/json; =b', 'application/json; a=b; c=d', 'application/json;a=b;',
    ' application/json', 'application/json ', ' application/json', 'application/json ; a=b',
    'application/json; a=é', 'application/json; a="é"', 'application/json; a="x	y"',
    'application/vnd.api+json', 'text/plain', 'application/jsonx', 'application/ json', 'application /json',
    'application', '/json', 'application/', ';', ' ; a=b', 'application/json;;', 'application/json; a=b ;c=d',
    'text/plain; x=","', 'text/plain, application/json',
];
const contentTypeResults = contentTypes.map((h) => ({ header: h, json: isJsonContentType(h) }));

const out = {
    tools: JSON.stringify(mcp.TOOLS),
    permissions: JSON.stringify(toolPermissions),
    validation,
    contentTypes: contentTypeResults,
};
fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify(out, null, 1) + '\n');
console.log(`wrote ${validation.length} validation cases`);
process.exit(0);
