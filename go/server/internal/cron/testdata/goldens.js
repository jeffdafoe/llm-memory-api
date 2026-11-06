// Writes goldens.json beside this file: node-cron's own verdicts and next
// matches for a set of expressions, so the Go port is compared with the
// library the Node server schedules with. Run from the repository root,
// after `npm ci` in node/api:
//
//     node go/server/internal/cron/testdata/goldens.js

const fs = require('fs');
const path = require('path');

const dist = path.resolve(__dirname, '../../../../../node/api/node_modules/node-cron/dist');
const cron = require(path.join(dist, 'node-cron.cjs'));
const { TimeMatcher } = require(path.join(dist, '_shared.cjs'));
const version = require(path.join(dist, '../package.json')).version;

const expressions = [
    // The seed values and plain forms.
    '0 5 * * *', '0 3 * * *', '*/15 * * * *', '* * * * * *', '0 0 * * 0', '5 4 * * sun', '0 22 * * 1-5',
    '0 0 1 * *', '0 0 1 1 *', '30 2 * * *', '30 1 * * *', '0 0 29 2 *', '0 0 31 * *', '0 0 30 2 *',
    '0 0 31 4,6 *', '0 0 31 2,3 *', '0 0 1,15 * 1',
    // Nicknames, spacing and case.
    '@daily', '@DAILY', ' @weekly ', '@hourly', '@monthly', '@yearly', '@annually', '@midnight', '@reboot', '@',
    '  0   5  *  *  *  ', '0 5 * * * ', '0\t5 * * *', '0 5 * *', '0 0 5 * * * *', '', ' ', '0', '* * * * * * *',
    // Names.
    '0 0 * jan,jul *', '0 0 * JANUARY *', '0 0 * * MON-FRI', '0 0 * * monday', '0 0 * * sat,sun', '0 0 * * tue-thu',
    '0 0 * janu *', '0 0 * * mo', '0 0 * dec-feb *', '0 0 * * fri-mon', '0 0 * * sunday-saturday', '0 0 * mayjun *',
    // Ranges, steps and wrap.
    '0 22-2 * * *', '0 0-23/5 * * *', '0-30/10 * * * * *', '*/7 * * * *', '0 */0 * * *', '0 1-5/0 * * *',
    '0 5/10 * * *', '0-70/100 * * * *', '50-70 * * * *', '0 25-2 * * *', '0 30-2 * * *', '0 0 30-2 * *',
    '0 0 * 11-2 *', '0 0 * * 5-1', '0 0 * * 0-7', '0 0 * * 7', '0 0 * * 6-7', '60 * * * *', '0 24 * * *',
    '0 0 0 * *', '0 0 32 * *', '0 0 * 0 *', '0 0 * 13 *', '0 0 * * 8', '**/5 * * * *', '*/*/5 * * * *',
    '1,,2 * * * *', '0 0 1-31/3 * *', '00 05 * * *', '0 0 * * 007', '99999999999999999999 * * * *',
    '0-99999999999 * * * *', '0 0 1-99999 * *',
    // Weekday ranges through 7 (Sunday) and past it.
    '0 0 * * 0-8', '0 0 * * 6-8', '0 0 * * 7-8', '0 0 * * 1-9/3', '0 0 * * 0-14/7', '0 0 * * 5-7/2', '0 0 * * 6-7/1',
    '0 0 * * 7-7', '0 0 * * 8-1', '0 0 * * 0-99999999999', '0 0 * 1-13 *', '0 0 * 12-13 *',
    // L, W, # and ?.
    '0 0 L * *', '0 0 l * *', '0 0 LW * *', '0 0 lw * *', '0 0 15W * *', '0 0 1W * *', '0 0 31W * *', '0 0 0W * *',
    '0 0 32W * *', '0 0 L-2 * *', '0 0 L-0 * *', '0 0 L-30 * *', '0 0 L-31 * *', '0 0 1-5W * *', '0 0 W * *',
    '0 0 15w,L * *', '0 0 * * 5L', '0 0 * * 5l', '0 0 * * 7L', '0 0 * * 8L', '0 0 * * 1#2', '0 0 * * 7#1',
    '0 0 * * 1#6', '0 0 * * 1#0', '0 0 * * 5#5', '0 0 ? * 1', '0 0 1 * ?', '0 0 ?,1 * *', '0 0 L * 5L',
    '0 0 L 2 *', '0 0 30W 2 *', '0 0 15 * 1#2', '0 L * * *', '0 0 * L *', '0 0 * * L', '0 0 1#2 * *',
    // Characters node-cron refuses.
    '0 0 * * * !', '0 0 * * 1;2', '0 0 * * ½', '0_5 * * *', '0 5 * * *\n',
];

// validateDetailed throws instead of reporting for a range too long to
// build ("0-99999999999"); validate() catches that and says false.
const validate = expressions.map((e) => {
    let error = '';
    try {
        const detailed = cron.validateDetailed(e);
        error = detailed.valid ? '' : detailed.errors[0].message;
    } catch (err) {
        error = 'threw: ' + err.message;
    }
    return { in: e, valid: cron.validate(e), error };
});

// Next matches: three in a row from each base instant, in several zones,
// including the 2026 DST changes (US 8 Mar / 1 Nov, EU 29 Mar / 25 Oct,
// Lord Howe's half hour 5 Apr / 4 Oct).
const zones = ['UTC', 'America/New_York', 'Europe/London', 'Australia/Lord_Howe', 'Asia/Kolkata'];
const bases = [
    '2026-01-01T00:00:00.000Z', '2026-02-27T23:59:59.999Z', '2026-03-08T06:29:30.500Z', '2026-03-29T00:30:00.000Z',
    '2026-04-04T14:00:00.000Z', '2026-10-25T00:30:00.000Z', '2026-11-01T05:00:00.000Z', '2026-11-06T15:14:07.441Z',
    '2027-12-31T23:59:59.000Z', '2028-02-28T12:00:00.000Z',
];
const nextPatterns = [
    '0 5 * * *', '*/15 * * * *', '30 2 * * *', '30 1 * * *', '0 0 L * *', '0 0 LW * *', '0 0 15W * *', '0 0 1W * *',
    '0 0 L-2 * *', '0 0 * * 5L', '0 0 * * 1#2', '0 0 29 2 *', '0 0 * * 7', '0 0 1 * MON', '@weekly',
    '*/20 * * * * *', '0 22-2 * * *', '0 0 * * MON-FRI', '0 0 31 * *', '0 0 1,15 * 1', '0 0 1 jan,jul *',
    '0 0 * * 5#5', '0 2-3 * * *', '15 * * * *', '0 0 30W 2 *',
];
const next = [];
for (const zone of zones) {
    for (const pattern of nextPatterns) {
        const matcher = new TimeMatcher(pattern, zone);
        for (const base of bases) {
            const out = [];
            let from = new Date(base);
            let error = '';
            for (let i = 0; i < 3; i++) {
                try {
                    from = matcher.getNextMatch(from);
                    out.push(from.toISOString());
                } catch (err) {
                    error = err.message;
                    break;
                }
            }
            next.push({ zone, pattern, base, next: out, error });
        }
    }
}

// match() at instants inside and around the DST changes.
const matchInstants = [
    '2026-03-08T06:30:00.000Z', '2026-03-08T07:30:00.000Z', '2026-11-01T05:30:00.000Z', '2026-11-01T06:30:00.000Z',
    '2026-10-04T15:30:00.000Z', '2026-02-27T05:00:00.000Z', '2026-03-31T00:00:00.000Z',
];
const matchPatterns = ['30 1 * * *', '30 2 * * *', '0 5 * * *', '0 0 L * *', '30 * * * *', '0 0 * * 5L'];
const match = [];
for (const zone of zones) {
    for (const pattern of matchPatterns) {
        const matcher = new TimeMatcher(pattern, zone);
        for (const at of matchInstants) {
            match.push({ zone, pattern, at, match: matcher.match(new Date(at)) });
        }
    }
}

fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify({ node_cron: version, validate, next, match }, null, 1) + '\n');
console.log(`wrote ${validate.length} validate, ${next.length} next and ${match.length} match cases (node-cron ${version})`);
