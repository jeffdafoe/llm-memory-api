// Writes goldens.json beside this file: the text node-postgres's
// prepareValue makes of each JSON value below, so the Go port's tests
// compare against node-postgres itself. Run from the repository root, after
// `npm ci` in node/api:
//
//     node go/server/internal/pgbind/testdata/goldens.js

const fs = require('fs');
const path = require('path');

const { prepareValue } = require(path.resolve(__dirname, '../../../../../node/api/node_modules/pg/lib/utils.js'));

const inputs = [
    '"plain"', '""', '"with \\"quotes\\" and \\\\ backslash"', '"ünï 😀"',
    '0', '-0', '7', '-12', '2.5', '1e21', '1e-7', '0.1', '123456789012345678901', '1.0',
    'true', 'false', 'null',
    '{}', '{"b":1,"a":[1,"x"]}', '{"2":"x","1":"y","k":null}',
    '[]', '[1,2]', '["a","b"]', '[null]', '[1,null,"x"]', '["q\\"uote","back\\\\slash"]',
    '[[1,2],[3]]', '[{"a":1}]', '[true,false]', '[-0,1e21]', '["{}"]', '[[]]',
];

const out = inputs.map(input => {
    const prepared = prepareValue(JSON.parse(input));
    return { input, prepared: prepared === null ? null : String(prepared) };
});

fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify(out, null, 2) + '\n');
console.log(`wrote ${out.length} goldens`);
