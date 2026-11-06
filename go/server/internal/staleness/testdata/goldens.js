// Writes goldens.json beside this file: what Node's staleness check
// functions (services/staleness.js, services/staleness-certificates.js)
// return for a set of inputs, so the Go port is compared with Node itself.
// Run from the repository root, after `npm ci` in node/api:
//
//     node go/server/internal/staleness/testdata/goldens.js
//
// Certificate cases are built in temporary directories; their paths are
// written as <root>/... with forward slashes.

const fs = require('fs');
const os = require('os');
const path = require('path');

const api = path.resolve(__dirname, '../../../../../node/api/src');
const s = require(path.join(api, 'services/staleness.js'));
const { collectCertificates } = require(path.join(api, 'services/staleness-certificates.js'));

function outcome(fn) {
    try {
        return { value: fn(), error: '' };
    } catch (err) {
        return { value: null, error: err.message };
    }
}

const aptOutputs = [
    '',
    'Reading package lists...\n0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n',
    [
        'Reading package lists...', 'Building dependency tree...', 'Calculating upgrade...',
        'The following packages will be upgraded:', '  libssl3t64 nodejs openssl tzdata',
        '4 upgraded, 1 newly installed, 0 to remove and 0 not upgraded.',
        'Inst libssl3t64 [3.5.1-1] (3.5.1-1+deb13u1 Debian-Security:13/stable-security [amd64])',
        'Inst openssl [3.5.1-1] (3.5.1-1+deb13u1 Debian:13.2/stable, Debian-Security:13/stable-security [amd64])',
        'Inst nodejs [24.22.0-1nodesource1] (24.22.1-1nodesource1 Node Source:nodistro [amd64])',
        'Inst tzdata [2025b-4] (2025c-0+deb13u1 Debian:13.2/stable [all])',
        'Inst libfoo1 (1.2-3 Debian:13.2/stable [amd64])',
        'Conf libssl3t64 (3.5.1-1+deb13u1 Debian-Security:13/stable-security [amd64])',
    ].join('\n'),
    'Inst foo [1.0] (1.1 [amd64])\n1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n',
    [
        'Inst vim [2:9.1.1230-2] (2:9.1.1230-2+deb13u1 Debian-Security:13/stable-security [amd64]) []',
        'Inst vim-common [2:9.1.1230-2] (2:9.1.1230-2+deb13u1 Debian-Security:13/stable-security [all]) [vim-tiny:amd64 ]',
        'Inst tzdata [2026e-0+deb13u1] (2026f-0+deb13u1 Debian:13.2/stable-updates [all])',
        '3 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.',
    ].join('\n'),
    'Remv libold1 [1.0-1] [libnew2:amd64 ]\n0 upgraded, 0 newly installed, 1 to remove and 0 not upgraded.\n',
    'Inst foo [1.0] (1.1 Debian:13.2/stable [amd64]) [] extra\n1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n',
    'Inst foo something new\n1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n',
    'Remv foo unexpected new format\n0 upgraded, 0 newly installed, 1 to remove and 0 not upgraded.\n',
    'Inst foo (1.1 Debian:13.2/stable [amd64])\n1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n',
    'Remv libold1 [1.0-1]\nInst libnew2 (2.0-1 Debian:13.2/stable [amd64])\n0 upgraded, 1 newly installed, 1 to remove and 3 not upgraded.',
    // CRLF, indentation, an empty old version, a lower-case origin, two
    // summaries (the last counts), a huge count, a Remv with no version.
    'Inst a [] (2 deb-SECURITY:x [amd64])\r\n  Inst b [1] (2 x:y [all])  \r\n1 upgraded, 1 newly installed, 0 to remove and 0 not upgraded.\r\n',
    '0 upgraded, 0 newly installed, 0 to remove and 1 not upgraded.\n0 upgraded, 0 newly installed, 0 to remove and 2 not upgraded.\n',
    '99999999999999999999999 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n',
    'Remv gone\n0 upgraded, 0 newly installed, 1 to remove and 0 not upgraded.\n',
    'Inst x [1] (2 Debian:13/stable, Other:1/a b c [amd64])\n1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n',
    'Inst x y [1] (2 [amd64])\n1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n',
    'Inst x [1] (2 a b [amd64])\n1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n',
    'Inst  x [1] (2 [amd64])\n1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n',
    'Inst x [1] (2 [amd64]) [a] [b]\n1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n',
    '1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded',
];
const apt = aptOutputs.map((input) => ({ in: input, out: s.parseAptSimulation(input) }));

const TODAYS = ['2026-10-21T12:30:00.000Z', '2026-12-31T23:59:59.999Z', '2028-02-29T00:00:00.000Z'];
const eolValues = [
    '"2028-04-30"', '"2027-01-19"', '"2027-01-20"', '"2026-04-30"', '"2026-10-21"', '"2026-10-22"', 'false', 'true',
    'null', null, '"soon"', '"2026-99-99"', '"2027-02-30"', '"2028-02-29"', '"2027-02-29"', '5', '{"a":1}', '[]',
    '"2026-10-21T00:00:00Z"', '"0000-01-01"', '"false"', '""', '"2026-1-01"', '"2026-02-00"', '"2026-04-31"',
];
const eol = [];
for (const today of TODAYS) {
    for (const raw of eolValues) {
        const value = raw === null ? undefined : JSON.parse(raw);
        eol.push({ label: 'Node.js', cycle: '24', eol: raw, today, out: outcome(() => s.assessEol('Node.js', '24', value, new Date(today))) });
    }
}

const DAY = 86400000;
const HOUR = 3600000;
const now = new Date('2026-10-21T12:30:00Z').getTime();
const certCases = [];
for (const [lifetime, left] of [
    [90, 50 * DAY], [90, 21 * DAY], [90, 21 * DAY + HOUR], [90, 21 * DAY - 1], [90, -129.5 * DAY], [90, -HOUR],
    [90, 0], [90, 1], [45, 14 * DAY], [45, 10 * DAY], [45, 10.5 * DAY], [365, 30 * DAY], [365, 20 * DAY],
    [1, 0.2 * DAY], [7, 1.66 * DAY], [90, -1], [90, 5.5 * DAY], [90, 2 * HOUR], [90, 1.5 * DAY], [90, -130.5 * DAY],
]) {
    const validTo = now + left;
    const validFrom = validTo - lifetime * DAY;
    certCases.push({ from: new Date(validFrom).toISOString(), to: new Date(validTo).toISOString(), now: new Date(now).toISOString() });
}
certCases.push({ from: new Date(now + HOUR).toISOString(), to: new Date(now + HOUR + 90 * DAY).toISOString(), now: new Date(now).toISOString() });
certCases.push({ from: new Date(now).toISOString(), to: new Date(now).toISOString(), now: new Date(now).toISOString() });
certCases.push({ from: new Date(now).toISOString(), to: new Date(now - DAY).toISOString(), now: new Date(now).toISOString() });
certCases.push({ from: new Date(now + HOUR).toISOString(), to: new Date(now + 90 * DAY).toISOString(), now: new Date(now + HOUR).toISOString() });
const cert = certCases.map((c) => ({ ...c, out: outcome(() => s.assessCertificate('example.org', new Date(c.from), new Date(c.to), new Date(c.now))) }));

const releases = [
    '6.12.113+deb13-amd64', '6.12.113+deb13-rt-amd64', '6.16.3+bpo-cloud-amd64', '6.1.0-25-amd64', 'foo',
    '6.12.113+deb13-amd64.old', '', '6+deb-amd64', '6.12+-amd64', '6.12.1+deb13~1.2-arm64', '6.12.1+DEB-amd64',
    '6.1.0-25-', '6.1.0-25-amd64-', '06.012.0113+deb13-amd64', '6.1-2-x',
];
const kernels = releases.map((r) => ({ in: r, out: s.parseKernelRelease(r) }));
const versionPairs = [
    [['6', '12', '113'], ['6', '12', '99']], [['6', '12', '99'], ['6', '12', '113']], [['6', '12', '111'], ['6', '12', '111']],
    [['7', '0', '1'], ['6', '12', '113']], [['6', '012', '114'], ['6', '12', '113']], [['90071992547409930'], ['90071992547409929']],
    [['6', '12'], ['6', '12', '0']], [['6', '12'], ['6', '12', '1']], [['000'], ['0']], [['0'], ['']], [[''], ['']], [['00'], ['1']],
];
const compare = versionPairs.map(([a, b]) => ({ a, b, sign: Math.sign(s.compareKernelVersions(a, b)) }));

const KERNELS = ['6.12.101+deb13-amd64', '6.12.111+deb13-amd64', '6.12.113+deb13-amd64'];
const rebootInputs = [
    {}, { runningKernel: '6.12.113+deb13-amd64', installedKernels: KERNELS },
    { runningKernel: '6.12.111+deb13-amd64', installedKernels: KERNELS },
    { runningKernel: '6.12.113+deb13-amd64', installedKernels: [...KERNELS, '6.12.120+deb13-rt-amd64', '6.12.120+deb13-cloud-amd64'] },
    { runningKernel: '6.12.113+deb13-amd64', installedKernels: [...KERNELS, 'foo', '9.9.9-garbage'] },
    { flagExists: false, flagPackages: 'libc6\n', runningKernel: '6.12.111+deb13-amd64', installedKernels: KERNELS },
    { flagExists: true, flagPackages: 'libc6\nlibc6\nlibssl3t64\n', runningKernel: '6.12.113+deb13-amd64', installedKernels: KERNELS },
    { flagExists: true, flagPackages: 'linux-image-6.12.113+deb13-amd64\n', runningKernel: '6.12.111+deb13-amd64', installedKernels: KERNELS },
    { flagExists: true, flagPackages: '  a \r\n\n b\n a', runningKernel: 'garbage', installedKernels: KERNELS },
    { flagExists: true, runningKernel: '6.12.111+deb13-amd64', installedKernels: [] },
    { runningKernel: '6.1.0-25-amd64', installedKernels: ['6.1.0-26-amd64', '6.1.0-27-amd64', '6.1.0-3-amd64', '6.12.1+deb13-amd64'] },
    { runningKernel: '6.12.111+deb13-amd64', installedKernels: ['6.12.113+deb13-amd64', '6.12.113+bpo-amd64'] },
];
const reboot = rebootInputs.map((input) => ({ in: input, out: s.assessReboot(input) }));

// Reports from combinations of the pieces above.
const fullApt = apt[2].out;
const certFindings = cert.map((c) => c.out.value).filter(Boolean);
const eolFindings = eol.map((e) => e.out.value).filter(Boolean);
const reboots = reboot.map((r) => r.out).filter(Boolean);
const reportInputs = [
    { host: 'host5' },
    { host: 'host5', upgrades: [], eolFindings: [], failures: [] },
    { host: 'host5', upgrades: fullApt.upgrades },
    { host: 'host5', removals: [{ name: 'libold1', from: '1.0-1' }] },
    { host: 'host5', removals: [{ name: 'gone', from: null }, { name: 'b', from: '' }] },
    { host: 'host5', heldBack: 2 },
    { host: 'host5', heldBack: 1 },
    { host: 'host5', eolFindings: eolFindings.slice(0, 1) },
    { host: 'host5', eolFindings },
    { host: 'host5', certFindings: certFindings.slice(0, 1) },
    { host: 'host5', certFindings, eolFindings: eolFindings.slice(0, 2) },
    { host: 'host5', failures: ['endoflife.date lookup for Debian 13 failed: HTTP 503'] },
    { host: 'host5', failures: ['a', 'b'] },
    { host: 'host5', reboot: reboots[0] },
    { host: 'host5', reboot: reboots[reboots.length - 1] },
    {
        host: 'vps.example', upgrades: [...fullApt.upgrades, ...apt[4].out.upgrades], removals: apt[5].out.removals, heldBack: 3,
        eolFindings: eolFindings.slice(0, 3), reboot: reboots[1], certFindings: certFindings.slice(0, 2), failures: ['x failed'],
    },
    { host: 'h', upgrades: [{ name: 'nodejs', from: '1', to: '2', origins: 'Node Source:nodistro', security: false }] },
    { host: 'h', upgrades: [{ name: 'nodejs', from: null, to: '2', origins: 'Debian-Security', security: true }] },
    { host: 'h', upgrades: apt[3].out.upgrades, failures: ['  trailing space  '] },
];
const report = reportInputs.map((input) => ({ in: input, out: s.buildReport(input) }));

// collectCertificates against temporary /etc/letsencrypt trees.
const PEM = fs.readFileSync(path.join(api, 'services/test-fixtures/example-cert.pem'), 'utf8');
const { X509Certificate } = require('crypto');
const fixture = new X509Certificate(PEM);
const validFrom = new Date(fixture.validFrom).getTime();
const validTo = new Date(fixture.validTo).getTime();
const moments = {
    healthy: new Date(validFrom + DAY).toISOString(),
    expiring: new Date(validTo - 5 * DAY).toISOString(),
    expired: new Date(validTo + 3 * DAY).toISOString(),
    before: new Date(validFrom - DAY).toISOString(),
};
// A layout: renewal names (or "file" for a file in its place), live names
// to "cert" / "bad" / "dir" (a directory named cert.pem) / "none" (no
// cert.pem), or "file" for live itself.
const layouts = [
    { name: 'empty root', renewal: null, live: null, at: 'healthy' },
    { name: 'healthy', renewal: ['example.org'], live: { 'example.org': 'cert' }, at: 'healthy' },
    { name: 'expiring', renewal: ['example.org'], live: { 'example.org': 'cert' }, at: 'expiring' },
    { name: 'expired', renewal: ['example.org'], live: { 'example.org': 'cert' }, at: 'expired' },
    { name: 'renewal without live cert', renewal: ['example.org', 'example.net'], live: { 'example.org': 'cert' }, at: 'healthy' },
    { name: 'no live directory', renewal: ['example.org'], live: null, at: 'healthy' },
    { name: 'no cert.pem, bad cert', renewal: ['example.org', 'example.net'], live: { 'example.org': 'none', 'example.net': 'bad' }, at: 'healthy' },
    { name: 'cert.pem is a directory', renewal: ['example.org'], live: { 'example.org': 'dir' }, at: 'healthy' },
    { name: 'live without renewal config', renewal: ['example.org'], live: { 'example.org': 'cert', 'example.net': 'cert' }, at: 'healthy' },
    { name: 'no renewal directory', renewal: null, live: { 'example.org': 'cert' }, at: 'healthy' },
    { name: 'renewal is a file', renewal: 'file', live: { 'example.org': 'cert' }, at: 'healthy' },
    { name: 'not valid yet', renewal: ['example.org'], live: { 'example.org': 'cert' }, at: 'before' },
    { name: 'directories with no certificates', renewal: [], live: {}, at: 'healthy' },
    // Reading live/x/cert.pem through a file is ENOENT on Windows, ENOTDIR on Linux.
    { name: 'live is a file', renewal: ['example.org'], live: 'file', at: 'healthy', platformDependent: true },
    { name: 'sorted names', renewal: ['b.org', 'a.org', 'c.org'], live: { 'c.org': 'cert', 'a.org': 'cert', 'b.org': 'cert' }, at: 'expiring' },
];
function build(root, layout) {
    if (layout.renewal === 'file') fs.writeFileSync(path.join(root, 'renewal'), '');
    else if (layout.renewal) {
        fs.mkdirSync(path.join(root, 'renewal'));
        for (const n of layout.renewal) fs.writeFileSync(path.join(root, 'renewal', `${n}.conf`), '');
        // Entries that are not renewal configs.
        fs.writeFileSync(path.join(root, 'renewal', 'notes.txt'), '');
        fs.mkdirSync(path.join(root, 'renewal', 'dir.conf'));
    }
    if (layout.live === 'file') fs.writeFileSync(path.join(root, 'live'), '');
    else if (layout.live) {
        fs.mkdirSync(path.join(root, 'live'));
        fs.writeFileSync(path.join(root, 'live', 'README'), '');
        for (const [n, kind] of Object.entries(layout.live)) {
            fs.mkdirSync(path.join(root, 'live', n));
            if (kind === 'cert') fs.writeFileSync(path.join(root, 'live', n, 'cert.pem'), PEM);
            if (kind === 'bad') fs.writeFileSync(path.join(root, 'live', n, 'cert.pem'), 'not a certificate');
            if (kind === 'dir') fs.mkdirSync(path.join(root, 'live', n, 'cert.pem'));
        }
    }
}
const certificates = [];
for (const layout of layouts) {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'staleness-golden-'));
    try {
        build(root, layout);
        const out = collectCertificates({ letsencryptDir: root, now: new Date(moments[layout.at]) });
        const clean = (t) => t.split(root).join('<root>').replace(/\\/g, '/');
        out.failures = out.failures.map(clean);
        certificates.push({ ...layout, now: moments[layout.at], out });
    } finally {
        fs.rmSync(root, { recursive: true, force: true });
    }
}

fs.writeFileSync(path.join(__dirname, 'goldens.json'), JSON.stringify({ platform: process.platform, apt, eol, cert, kernels, compare, reboot, report, pem: PEM, certificates }, null, 1) + '\n');
console.log(`wrote ${apt.length} apt, ${eol.length} eol, ${cert.length} cert, ${kernels.length} kernel, ${reboot.length} reboot, ${report.length} report and ${certificates.length} certificate-tree cases`);
