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
    const upgrades = parseAptSimulation(APT_OUTPUT);
    assert.deepEqual(upgrades.map((u) => u.name), ['libssl3t64', 'openssl', 'nodejs', 'tzdata', 'libfoo1']);
});

test('parseAptSimulation keeps old and new versions, and marks new packages', () => {
    const [libssl, , nodejs, , libfoo] = parseAptSimulation(APT_OUTPUT);
    assert.equal(libssl.from, '3.5.1-1');
    assert.equal(libssl.to, '3.5.1-1+deb13u1');
    assert.equal(nodejs.from, '24.22.0-1nodesource1');
    assert.equal(nodejs.to, '24.22.1-1nodesource1');
    assert.equal(libfoo.from, null);
    assert.equal(libfoo.to, '1.2-3');
});

test('parseAptSimulation flags security from any of several origins', () => {
    const byName = Object.fromEntries(parseAptSimulation(APT_OUTPUT).map((u) => [u.name, u.security]));
    assert.deepEqual(byName, { libssl3t64: true, openssl: true, nodejs: false, tzdata: false, libfoo1: false });
});

test('parseAptSimulation of a clean system is empty', () => {
    assert.deepEqual(parseAptSimulation('Reading package lists...\n0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n'), []);
    assert.deepEqual(parseAptSimulation(''), []);
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

test('buildReport sends nothing on a clean week', () => {
    assert.equal(buildReport({ host: 'host5' }), null);
    assert.equal(buildReport({ host: 'host5', upgrades: [], eolFindings: [], failures: [] }), null);
});

test('buildReport groups security, Node.js and other updates', () => {
    const report = buildReport({ host: 'host5', upgrades: parseAptSimulation(APT_OUTPUT) });
    assert.equal(report.subject, 'Staleness check (host5): 2 security updates, Node.js update, 2 other updates');
    const security = report.body.indexOf('## Security updates');
    const node = report.body.indexOf('## Node.js');
    const other = report.body.indexOf('## Other updates');
    assert.ok(security >= 0 && security < node && node < other);
    assert.match(report.body, /`libssl3t64` 3\.5\.1-1 → 3\.5\.1-1\+deb13u1/);
    assert.match(report.body, /`libfoo1` new: 1\.2-3/);
    assert.match(report.body, /restart `memory-api`/);
    assert.match(report.body, /sudo apt upgrade/);
    assert.match(report.body, /Nothing was installed/);
});

test('buildReport mails an end-of-life warning on its own', () => {
    const finding = assessEol('Node.js', '20', '2026-04-30', TODAY);
    const report = buildReport({ host: 'host5', eolFindings: [finding] });
    assert.equal(report.subject, 'Staleness check (host5): 1 end-of-life warning');
    assert.match(report.body, /Node\.js 20: support ENDED on 2026-04-30, 174 days ago\./);
    assert.doesNotMatch(report.body, /apt upgrade/);
});

test('buildReport mails a failed check, so a broken check never reads as clean', () => {
    const report = buildReport({ host: 'host5', failures: ['endoflife.date lookup for Debian 13 failed: HTTP 503'] });
    assert.equal(report.subject, 'Staleness check (host5): 1 failed check');
    assert.match(report.body, /## Checks that failed\n\n- endoflife\.date lookup for Debian 13 failed: HTTP 503/);
});
