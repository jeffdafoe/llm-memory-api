// Reads the server's Let's Encrypt certificates for the weekly staleness
// check (scripts/staleness-check.js). Kept apart from staleness.js, which is
// pure, so this file-reading part can be tested against temporary directories.

const fs = require('fs');
const path = require('path');
const { X509Certificate } = require('crypto');
const { assessCertificate } = require('./staleness');

// certbot keeps a renewal config `renewal/<name>.conf` for every certificate
// it manages, and a directory `live/<name>/` whose cert.pem links to the
// current issue. Both are listed, and every name found in either must have a
// readable live/<name>/cert.pem — so a certificate missing from one side is a
// failed check, never a certificate silently left out.
const SOURCES = [
    { dir: 'renewal', pick: (entry) => (entry.isFile() && entry.name.endsWith('.conf') ? entry.name.slice(0, -'.conf'.length) : null) },
    { dir: 'live', pick: (entry) => (entry.isDirectory() ? entry.name : null) },
];

// Returns { findings, failures, checked }: findings from assessCertificate,
// failed checks as report lines, and how many certificates were read.
// When neither directory exists the server does not use Let's Encrypt, and
// there is nothing to check; any other error listing them is a failure.
function collectCertificates({ letsencryptDir, now }) {
    const findings = [];
    const failures = [];
    const names = new Set();
    let listed = false;

    for (const source of SOURCES) {
        const dir = path.join(letsencryptDir, source.dir);
        let entries;
        try {
            entries = fs.readdirSync(dir, { withFileTypes: true });
        } catch (err) {
            if (err.code !== 'ENOENT') failures.push(`Could not list the TLS certificates in ${dir}: ${err.message}`);
            continue;
        }
        listed = true;
        for (const entry of entries) {
            const name = source.pick(entry);
            if (name) names.add(name);
        }
    }
    if (listed && names.size === 0) {
        failures.push(`${letsencryptDir} holds no certificates, so none were checked.`);
    }

    let checked = 0;
    for (const name of [...names].sort()) {
        try {
            // X509Certificate reads the first certificate in the file.
            const cert = new X509Certificate(fs.readFileSync(path.join(letsencryptDir, 'live', name, 'cert.pem')));
            const finding = assessCertificate(name, new Date(cert.validFrom), new Date(cert.validTo), now);
            checked++;
            if (finding) findings.push(finding);
        } catch (err) {
            failures.push(`Could not read the TLS certificate \`${name}\`: ${err.message}`);
        }
    }
    return { findings, failures, checked };
}

module.exports = { collectCertificates };
