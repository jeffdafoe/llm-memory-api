// Writes goldens.json beside this file: what the Node server's memory-sync
// helpers return for a set of inputs, so the Go port's unit tests compare
// against Node itself. Run from the repository root, after `npm ci` in
// node/api:
//
//     node go/server/internal/agent/testdata/goldens.js
//
// isSafeFilename and extractTitle are not exported by routes/agent.js, so
// their source is read from the module text and compiled here. The date
// cases run in America/New_York, so a date-time without an offset (local
// time) and the DST edges are pinned too.

process.env.TZ = 'America/New_York';

const fs = require('fs');
const path = require('path');

const source = fs.readFileSync(path.resolve(__dirname, '../../../../../node/api/src/routes/agent.js'), 'utf8');

function extract(name) {
    const match = source.match(new RegExp('\\nfunction ' + name + '\\([\\s\\S]*?\\n}\\n'));
    if (!match) {
        throw new Error('function ' + name + ' not found in routes/agent.js');
    }
    return match[0];
}

const helpers = new Function(extract('isSafeFilename') + extract('extractTitle') + 'return { isSafeFilename, extractTitle };')();

// The route's mtime check and comparison value: `!file.mtime ||
// isNaN(new Date(file.mtime).getTime())` and `new Date(local.mtime).getTime()`.
const mtimes = [
    '"2026-11-06T15:07:00.1234567Z"', '"2026-11-06T15:07:00Z"', '"2026-11-06T15:07:00.123Z"', '"2026-11-06T15:07:00.1Z"',
    '"2026-11-06T15:07:00.12Z"', '"2026-11-06T15:07:00.123456789Z"', '"2026-11-06T15:07:00.9999Z"', '"2026-11-06T15:07Z"',
    '"2026-11-06T15:07"', '"2026-11-06T15:07:00"', '"2026-11-06T15:07:00.5"', '"2026-11-06"', '"2026-11"', '"2026"',
    '"+002026-11-06T00:00:00Z"', '"-000001-01-01T00:00:00Z"', '"-000000-01-01T00:00:00Z"', '"+002026"', '"0000-01-01T00:00:00Z"',
    '"2026-11-06T15:07:00+01:00"', '"2026-11-06T15:07:00-05:00"', '"2026-11-06T15:07:00-0500"', '"2026-11-06T15:07:00+01"',
    '"2026-11-06T15:07:00.123+01:00"', '"2026-11-06T15:07:00+24:00"', '"2026-11-06T15:07:00+23:59"', '"2026-11-06T15:07:00+00:60"',
    '"2026-11-06 15:07:00"', '"2026-11-06 15:07:00Z"', '"2026-11-06t15:07:00z"', '"2026-11-06T15:07:00z"', '"2026-11-06t15:07:00Z"',
    '"2026-02-30"', '"2026-02-29"', '"2024-02-29"', '"2026-04-31"', '"2026-13-01"', '"2026-00-01"', '"2026-11-00"',
    '"2026-02-30T00:00:00Z"', '"2026-11-06T24:00:00Z"', '"2026-11-06T24:00:00.000Z"', '"2026-11-06T24:00:01Z"', '"2026-11-06T24:00"',
    '"2026-11-06T23:60:00Z"', '"2026-11-06T23:59:60Z"', '"2026-11-06T25:00:00Z"', '"2026-11-06T24:00:00.5Z"', '"2026-11-06T24:00:00.0Z"',
    '"2026-11-06T15:07:00.000+00:00"', '"2026-11-06T15:07:00-00:00"', '"2026-11-06T15:07:00.123456Z"', '"2026-07-01T12:00:00"',
    '"+012345-07-01T00:00:00"', '"-000001-01-01"', '"2026-11-06T15:07:00.1234567+05:30"', '"2026-11-06T15:07:00Zjunk"',
    '"10/09/2026"', '"Oct 9 2026"', '"Fri, 06 Nov 2026 15:07:00 GMT"', '"2026/11/06"', '""', '" "', '"garbage"', '"1700000000000"', '"2026x"',
    '"2026-11-06T15:07:00.Z"', '"2026-11-06T15:07:00,123Z"', '"2026-11-6"', '"026-11-06"', '"20261106"', '"2026-11-06T"', '"2026-11-06T15"',
    '"2026-11-06T15:7"', '"2026-11-06T15:07:0"', '"+275760-09-13T00:00:00.000Z"', '"+275760-09-13T00:00:00.001Z"', '"-271821-04-20T00:00:00Z"',
    '"-271821-04-19T23:59:59.999Z"', '"+275760-09-13"', '"+275761-01-01"', '"12345-01-01"', '"+12345-01-01"',
    '" 2026-11-06T15:07:00Z"', '"2026-11-06T15:07:00Z "', '"2026-11-06T15:07:00Z\\n"',
    '"2026-03-08T02:30:00"', '"2026-03-08T03:30:00"', '"2026-03-08T01:59:59.999"', '"2026-11-01T01:30:00"', '"2026-11-01T00:59:59"',
    '"2026-11-01T02:00:00"', '"1970-01-01T00:00:00Z"', '"1969-12-31T23:59:59.999Z"', '"0001-01-01T00:00:00Z"', '"1800-06-01T12:00:00"',
    '"\\u0032026-11-06"', '"２026-11-06"',
    '1', '0', '-1', '1.5', '-1.5', '1e20', '8640000000000000', '8640000000000001', '-8640000000000000', '-8640000000000001',
    '1700000000000', '1700000000000.9', '-0', '1e400',
    'true', 'false', 'null', '[]', '[1700000000000]', '["2026-11-06T15:07:00Z"]', '[["2026"]]', '[null]', '[1, 2]',
    '{}', '{"toString": 1}', '[{"toString": 1}]', '{"valueOf": 1}', '"0"',
];

function dateOutcome(text) {
    const value = JSON.parse(text);
    try {
        const ms = new Date(value).getTime();
        return { in: text, truthy: !!value, ms: isNaN(ms) ? null : ms };
    } catch (err) {
        return { in: text, truthy: !!value, error: err.message };
    }
}

const filenames = [
    '"foo.md"', '"foo"', '""', '"."', '".."', '".hidden"', '"a/b"', '"a\\\\b"', '"..md"', '"a..b"', '"x."', '" "', '"-"',
    '"constructor"', '"__proto__"', '"日本.md"', '"a\\u0000b"', 'null', '1', 'true', '["a"]', '{}', '"/"', '"\\\\"',
];

const titles = [
    ['---\nname: Some Title\n---\nbody', 'f.md'],
    ['---\nname:   Padded Title   \n---\n', 'f.md'],
    ['---\ntype: x\nname: Second Line\ndescription: y\n---\n', 'f.md'],
    ['---\nusername: alice\n---\n', 'f.md'],
    ['---\nname: No Close\n', 'f.md'],
    ['---\nname: Last line no newline---', 'f.md'],
    ['---   \nname: Spaces after dashes\n---', 'f.md'],
    ['---\r\nname: CRLF\r\n---\r\n', 'f.md'],
    ['---\nname: CR in value\r\n---\n', 'f.md'],
    ['---\nname:\n\nAfter Blank\n---\n', 'f.md'],
    ['---\nname:  nbsp \n---\n', 'f.md'],
    ['--- name: ls\n---', 'f.md'],
    ['---\nname: a b\n---', 'f.md'],
    ['---\nname: first\nname: second\n---', 'f.md'],
    ['---\n---\nname: after close\n---', 'f.md'],
    ['---\nname: x\n', 'f.md'],
    ['---\nname: x\n--', 'f.md'],
    [' ---\nname: lead space\n---', 'f.md'],
    ['# Heading One\nbody', 'f.md'],
    ['intro\n# Later Heading\n', 'f.md'],
    ['intro\r# After CR\n', 'f.md'],
    ['intro # After LS\n', 'f.md'],
    ['#NoSpace\n# With Space', 'f.md'],
    ['## Two hashes\n', 'f.md'],
    ['#\n\nHeading After Blank', 'f.md'],
    ['#   \n', 'f.md'],
    ['# Trailing spaces   \n', 'f.md'],
    ['# Title\r\nbody', 'f.md'],
    ['# nbsp heading', 'f.md'],
    ['# 日本語 😀', 'f.md'],
    ['---\nname: Frontmatter Wins\n---\n# Heading', 'f.md'],
    ['plain text', 'my_note-name.md'],
    ['', 'my_note-name.md'],
    ['', 'file.md.md'],
    ['', 'file.MD'],
    ['', 'noext'],
    ['', 'a.mdx'],
    ['', '__-__'],
    ['   ', 'blank_title.md'],
];

const out = {
    is_safe_filename: filenames.map(text => ({ in: text, out: helpers.isSafeFilename(JSON.parse(text)) })),
    extract_title: titles.map(([content, filename]) => ({ content, filename, out: helpers.extractTitle(content, filename) })),
    dates: mtimes.map(dateOutcome),
};

fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify(out, null, 2) + '\n');
