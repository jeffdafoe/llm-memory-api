// Reads the server's Let's Encrypt certificates for the weekly staleness
// check (scripts/staleness-check.js). Kept apart from staleness.js, which is
// pure, so this file-reading part can be tested against temporary directories.

const fs = require('fs');
const path = require('path');
const { X509Certificate } = require('crypto');
const { assessCertificate } = require('./staleness');

// certbot keeps a renewal config `renewal/<name>.conf` for every certificate
// it manages, and a directory `live/<name>/` whose cert.pem links to the
// current issue. Both are listed. A name in renewal/ with no readable
// live/<name>/cert.pem fails on the read; a name in live/ with no renewal
// config is reported too, because certbot will never renew it.
const SOURCES = {
    renewal: (entry) => (entry.isFile() && entry.name.endsWith('.conf') ? entry.name.slice(0, -'.conf'.length) : null),
    live: (entry) => (entry.isDirectory() ? entry.name : null),
};

// Returns { findings, failures, checked }: findings from assessCertificate,
// failed checks as report lines, and how many certificates were read.
// When neither directory exists the server does not use Let's Encrypt, and
// there is nothing to check; any other error listing them is a failure.
function collectCertificates({ letsencryptDir, now }) {
    const findings = [];
    const failures = [];
    // Per source: a Set of names, or null when the directory does not exist
    // or could not be listed.
    const found = {};
    let renewalUnlisted = false;
    for (const [source, pick] of Object.entries(SOURCES)) {
        const dir = path.join(letsencryptDir, source);
        found[source] = null;
        let entries;
        try {
            entries = fs.readdirSync(dir, { withFileTypes: true });
        } catch (err) {
            if (err.code !== 'ENOENT') {
                failures.push(`Could not list the TLS certificates in ${dir}: ${err.message}`);
                if (source === 'renewal') renewalUnlisted = true;
            }
            continue;
        }
        found[source] = new Set(entries.map(pick).filter(Boolean));
    }

    const names = new Set([...(found.renewal || []), ...(found.live || [])]);
    if ((found.renewal || found.live) && names.size === 0) {
        failures.push(`${letsencryptDir} holds no certificates, so none were checked.`);
    }
    // When renewal/ could not be listed, that failure already covers every
    // certificate; naming each one again would only repeat it.
    if (found.live && !renewalUnlisted) {
        for (const name of [...found.live].sort()) {
            if (!found.renewal || !found.renewal.has(name)) {
                failures.push(`The TLS certificate \`${name}\` has no renewal config in ${path.join(letsencryptDir, 'renewal')}, so certbot will not renew it.`);
            }
        }
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
