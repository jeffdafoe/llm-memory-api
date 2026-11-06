// Goldens for pgjson.Parse: node-postgres's own text parsers (pg-types, as
// node/api has it installed) over fixed values, written as JSON.stringify
// writes each result. Run from the repository root, after npm ci in
// node/api, with the zone the Go test uses:
//
//   TZ=America/New_York node go/server/internal/pgjson/testdata/goldens.js
//
// The output replaces testdata/goldens.json.

const path = require('path');
const fs = require('fs');

const types = require(path.join(__dirname, '../../../../../node/api/node_modules/pg-types'));

const cases = [
    [16, 't'], [16, 'f'], [16, 'true'], [16, 'yes'], [16, 'on'], [16, '1'], [16, 'TRUE'], [16, 'x'],
    [20, '9007199254740993'], [20, '-5'], [20, '0'],
    [21, '-32768'], [23, '2147483647'], [23, '-1'], [26, '4294967295'],
    [700, '1.5'], [700, '-0'], [700, 'NaN'], [700, 'Infinity'], [701, '1e+20'], [701, '1e21'],
    [701, '0.1'], [701, '-Infinity'], [701, '1.7976931348623157e+308'], [701, '5e-324'],
    [1700, '12.3400'], [1700, 'NaN'],
    [25, 'plain text'], [1043, ''], [2950, '6d2a0c1e-0000-4000-8000-0000000000a1'],
    [114, '{"b": 1, "a": [1, 2.50, "x"]}'], [3802, '{"a": 1, "b": {"c": null}}'], [114, '"str"'], [3802, '12'],
    [114, '{"1": "x", "a": 2, "0": "y"}'],
    [1082, '2026-11-06'], [1082, '2026-03-08'], [1082, '2026-11-01'], [1082, 'infinity'],
    [1114, '2026-11-06 12:01:43.086123'], [1114, '2026-03-08 02:30:00'], [1114, '2026-11-01 01:30:00'],
    [1184, '2026-11-06 17:01:43.086123+00'], [1184, '2026-11-06 12:01:43.0869+05:30'], [1184, '2026-11-06 12:01:43-05'],
    [1184, '2026-11-06 12:01:43.999999+00'], [1184, 'infinity'], [1184, '-infinity'],
    [1000, '{t,f,NULL}'], [1005, '{1,-2}'], [1007, '{1,2,NULL,3}'], [1007, '{}'], [1007, '{{1,2},{3,4}}'],
    [1016, '{9007199254740993,1}'], [1021, '{1.5,NaN}'], [1022, '{0.1,Infinity}'], [1231, '{1.50,2}'],
    [1009, '{a,"b c","d\\"e","f\\\\g",NULL,"NULL",""}'], [1015, '{x}'], [1014, '{"a "}'],
    [1185, '{"2026-11-06 17:01:43.086+00",NULL}'], [1182, '{2026-11-06}'], [1115, '{"2026-11-06 12:00:00"}'],
    [199, '{"{\\"a\\":1}",NULL}'], [3807, '{"[1,2]"}'],
    [2951, '{6d2a0c1e-0000-4000-8000-0000000000a1}'],
    [1009, '[1:2]={a,b}'],
];

const out = cases.map(([oid, text]) => {
    const parsed = types.getTypeParser(oid, 'text')(text);
    return { oid, text, json: JSON.stringify(parsed) };
});

fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify(out, null, 2) + '\n');
