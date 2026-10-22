// Tests for the weekly staleness check's decisions (staleness.js). Run with:
// node --test (from node/api). Pure functions — no shell, network or DB.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { parseAptSimulation, assessEol, buildReport, EOL_WARN_DAYS } = require('./staleness');

// Shaped like real `apt-get -s dist-upgrade` output on Debian 13: a summary
// header, Inst lines (security, NodeSource, multi-origin, a brand-new
// dependency with no old version), and the Conf lines apt prints after them.
const APT_OUTPUT = [
    'Reading package lists...',
    'Building dependency tree...',
    'Calculating upgrade...',
    'The following packages will be upgraded:',
    '  libssl3t64 nodejs openssl tzdata',
    '4 upgraded, 1 newly installed, 0 to remove and 0 not upgraded.',
    'Inst libssl3t64 [3.5.1-1] (3.5.1-1+deb13u1 Debian-Security:13/stable-security [amd64])',
    'Inst openssl [3.5.1-1] (3.5.1-1+deb13u1 Debian:13.2/stable, Debian-Security:13/stable-security [amd64])',
    'Inst nodejs [24.22.0-1nodesource1] (24.22.1-1nodesource1 Node Source:nodistro [amd64])',
    'Inst tzdata [2025b-4] (2025c-0+deb13u1 Debian:13.2/stable [all])',
    'Inst libfoo1 (1.2-3 Debian:13.2/stable [amd64])',
    'Conf libssl3t64 (3.5.1-1+deb13u1 Debian-Security:13/stable-security [amd64])',
    'Conf nodejs (24.22.1-1nodesource1 Node Source:nodistro [amd64])',
].join('\n');

const TODAY = new Date('2026-10-21T12:30:00Z');

test('parseAptSimulation reads every Inst line and ignores the rest', () => {
    const { upgrades, removals, heldBack, problems } = parseAptSimulation(APT_OUTPUT);
    assert.deepEqual(upgrades.map((u) => u.name), ['libssl3t64', 'openssl', 'nodejs', 'tzdata', 'libfoo1']);
    assert.deepEqual(removals, []);
    assert.equal(heldBack, 0);
    assert.deepEqual(problems, []);
});

test('parseAptSimulation keeps old and new versions, and marks new packages', () => {
    const [libssl, , nodejs, , libfoo] = parseAptSimulation(APT_OUTPUT).upgrades;
    assert.equal(libssl.from, '3.5.1-1');
    assert.equal(libssl.to, '3.5.1-1+deb13u1');
    assert.equal(nodejs.from, '24.22.0-1nodesource1');
    assert.equal(nodejs.to, '24.22.1-1nodesource1');
    assert.equal(libfoo.from, null);
    assert.equal(libfoo.to, '1.2-3');
});

test('parseAptSimulation flags security from any of several origins', () => {
    const byName = Object.fromEntries(parseAptSimulation(APT_OUTPUT).upgrades.map((u) => [u.name, u.security]));
    assert.deepEqual(byName, { libssl3t64: true, openssl: true, nodejs: false, tzdata: false, libfoo1: false });
});

test('parseAptSimulation of a clean system is empty with no problems', () => {
    const clean = parseAptSimulation('Reading package lists...\n0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n');
    assert.deepEqual(clean, { upgrades: [], removals: [], heldBack: 0, problems: [] });
});

test('parseAptSimulation: empty output is a problem, not a clean system', () => {
    const { upgrades, problems } = parseAptSimulation('');
    assert.deepEqual(upgrades, []);
    assert.equal(problems.length, 1);
    assert.match(problems[0], /no "N upgraded/);
});

test('parseAptSimulation reads a package with no origin', () => {
    const output = 'Inst foo [1.0] (1.1 [amd64])\n1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n';
    const { upgrades, problems } = parseAptSimulation(output);
    assert.deepEqual(upgrades, [{ name: 'foo', from: '1.0', to: '1.1', origins: '', security: false }]);
    assert.deepEqual(problems, []);
});

test('parseAptSimulation reports an Inst line it cannot read, and the count mismatch', () => {
    const output = 'Inst foo something new\n1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n';
    const { upgrades, problems } = parseAptSimulation(output);
    assert.deepEqual(upgrades, []);
    assert.equal(problems.length, 2);
    assert.match(problems[0], /unrecognised apt line: Inst foo something new/);
    assert.match(problems[1], /does not match the lines read \(0 upgraded, 0 newly installed, 0 to remove\)/);
});

test('parseAptSimulation reports a Remv line it cannot read', () => {
    const output = 'Remv foo unexpected new format\n0 upgraded, 0 newly installed, 1 to remove and 0 not upgraded.\n';
    const { removals, problems } = parseAptSimulation(output);
    assert.deepEqual(removals, []);
    assert.match(problems[0], /unrecognised apt line: Remv foo unexpected new format/);
});

test('parseAptSimulation checks the upgraded / newly-installed split, not just the total', () => {
    // One new package (no "[old]"), but apt says it is an upgrade.
    const output = 'Inst foo (1.1 Debian:13.2/stable [amd64])\n1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n';
    const { problems } = parseAptSimulation(output);
    assert.equal(problems.length, 1);
    assert.match(problems[0], /\(0 upgraded, 1 newly installed, 0 to remove\)/);
});

test('parseAptSimulation reads removals and the held-back count', () => {
    const output = [
        'Remv libold1 [1.0-1]',
        'Inst libnew2 (2.0-1 Debian:13.2/stable [amd64])',
        '0 upgraded, 1 newly installed, 1 to remove and 3 not upgraded.',
    ].join('\n');
    const { upgrades, removals, heldBack, problems } = parseAptSimulation(output);
    assert.deepEqual(upgrades.map((u) => u.name), ['libnew2']);
    assert.deepEqual(removals, [{ name: 'libold1', from: '1.0-1' }]);
    assert.equal(heldBack, 3);
    assert.deepEqual(problems, []);
});

test('assessEol: far-off end date is not a finding', () => {
    assert.equal(assessEol('Node.js', '24', '2028-04-30', TODAY), null);
});

test('assessEol: the warn window is inclusive at EOL_WARN_DAYS', () => {
    // 2027-01-19 is exactly 90 days after 2026-10-21.
    assert.equal(EOL_WARN_DAYS, 90);
    const atEdge = assessEol('Node.js', '22', '2027-01-19', TODAY);
    assert.deepEqual(atEdge, { label: 'Node.js', cycle: '22', eol: '2027-01-19', daysLeft: 90, ended: false });
    assert.equal(assessEol('Node.js', '22', '2027-01-20', TODAY), null);
});

test('assessEol: a passed date is an ended finding with negative days', () => {
    const finding = assessEol('Node.js', '20', '2026-04-30', TODAY);
    assert.equal(finding.ended, true);
    assert.equal(finding.daysLeft, -174);
});

test('assessEol: false means no announced end, true means ended without a date', () => {
    assert.equal(assessEol('PostgreSQL', '17', false, TODAY), null);
    assert.deepEqual(assessEol('Debian', '9', true, TODAY), { label: 'Debian', cycle: '9', eol: null, daysLeft: null, ended: true });
});

test('assessEol rejects a value it does not understand', () => {
    assert.throws(() => assessEol('Node.js', '24', 'soon', TODAY), /unexpected eol value/);
});

test('assessEol rejects an impossible calendar date', () => {
    assert.throws(() => assessEol('Node.js', '24', '2026-99-99', TODAY), /not a calendar date/);
    // Date.parse would roll Feb 30 over to Mar 2; the round-trip check catches it.
    assert.throws(() => assessEol('Node.js', '24', '2027-02-30', TODAY), /not a calendar date/);
});

test('buildReport sends nothing on a clean week', () => {
    assert.equal(buildReport({ host: 'host5' }), null);
    assert.equal(buildReport({ host: 'host5', upgrades: [], eolFindings: [], failures: [] }), null);
});

test('buildReport groups security, Node.js and other updates', () => {
    const report = buildReport({ host: 'host5', upgrades: parseAptSimulation(APT_OUTPUT).upgrades });
    assert.equal(report.subject, 'Staleness check (host5): 2 security updates, Node.js update, 2 other updates');
    const security = report.body.indexOf('## Security updates');
    const node = report.body.indexOf('## Node.js');
    const other = report.body.indexOf('## Other updates');
    assert.ok(security >= 0 && security < node && node < other);
    assert.match(report.body, /`libssl3t64` 3\.5\.1-1 → 3\.5\.1-1\+deb13u1/);
    assert.match(report.body, /`libfoo1` new: 1\.2-3/);
    assert.match(report.body, /restart `memory-api`/);
    assert.match(report.body, /sudo apt full-upgrade/);
    assert.match(report.body, /Nothing was installed/);
});

test('buildReport mails removals and held-back packages even with nothing to install', () => {
    const removalsOnly = buildReport({ host: 'host5', removals: [{ name: 'libold1', from: '1.0-1' }] });
    assert.equal(removalsOnly.subject, 'Staleness check (host5): 1 removal');
    assert.match(removalsOnly.body, /## Packages the upgrade would remove\n\n- `libold1` 1\.0-1/);
    assert.match(removalsOnly.body, /sudo apt full-upgrade/);

    const heldOnly = buildReport({ host: 'host5', heldBack: 2 });
    assert.equal(heldOnly.subject, 'Staleness check (host5): 2 held back');
    assert.match(heldOnly.body, /2 packages have updates that the upgrade would not install/);
    assert.doesNotMatch(heldOnly.body, /full-upgrade/);
});

test('buildReport mails an end-of-life warning on its own', () => {
    const finding = assessEol('Node.js', '20', '2026-04-30', TODAY);
    const report = buildReport({ host: 'host5', eolFindings: [finding] });
    assert.equal(report.subject, 'Staleness check (host5): 1 end-of-life warning');
    assert.match(report.body, /Node\.js 20: support ENDED on 2026-04-30, 174 days ago\./);
    assert.doesNotMatch(report.body, /full-upgrade/);
});

test('buildReport mails a failed check, so a broken check never reads as clean', () => {
    const report = buildReport({ host: 'host5', failures: ['endoflife.date lookup for Debian 13 failed: HTTP 503'] });
    assert.equal(report.subject, 'Staleness check (host5): 1 failed check');
    assert.match(report.body, /## Checks that failed\n\n- endoflife\.date lookup for Debian 13 failed: HTTP 503/);
});
