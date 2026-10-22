// Tests for reading the Let's Encrypt certificates (staleness-certificates.js)
// against temporary directories shaped like /etc/letsencrypt. Run with:
// node --test (from node/api).

const { test, after } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { X509Certificate } = require('crypto');
const { collectCertificates } = require('./staleness-certificates');

// A self-signed 90-day certificate for example.org (no private key kept).
const CERT_PEM = fs.readFileSync(path.join(__dirname, 'test-fixtures', 'example-cert.pem'));
const CERT = new X509Certificate(CERT_PEM);
const VALID_TO = new Date(CERT.validTo);
const DAY_MS = 86400000;
// Moments in the fixture's life: well inside it, and 5 days before expiry.
const HEALTHY = new Date(new Date(CERT.validFrom).getTime() + DAY_MS);
const EXPIRING = new Date(VALID_TO.getTime() - 5 * DAY_MS);

const roots = [];
after(() => {
    for (const root of roots) fs.rmSync(root, { recursive: true, force: true });
});

// Builds a temporary /etc/letsencrypt: `renewal` names get a .conf file,
// `live` maps a name to its cert.pem contents (null: directory, no cert.pem).
function letsencrypt({ renewal = null, live = null } = {}) {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), 'staleness-certs-'));
    roots.push(root);
    if (renewal) {
        fs.mkdirSync(path.join(root, 'renewal'));
        for (const name of renewal) fs.writeFileSync(path.join(root, 'renewal', `${name}.conf`), '');
    }
    if (live) {
        fs.mkdirSync(path.join(root, 'live'));
        // certbot leaves a README beside the certificate directories.
        fs.writeFileSync(path.join(root, 'live', 'README'), '');
        for (const [name, pem] of Object.entries(live)) {
            fs.mkdirSync(path.join(root, 'live', name));
            if (pem !== null) fs.writeFileSync(path.join(root, 'live', name, 'cert.pem'), pem);
        }
    }
    return root;
}

test('collectCertificates: no Let\'s Encrypt directories means nothing to check', () => {
    const root = letsencrypt();
    assert.deepEqual(collectCertificates({ letsencryptDir: root, now: HEALTHY }), { findings: [], failures: [], checked: 0 });
});

test('collectCertificates reads a healthy certificate and reports nothing', () => {
    const root = letsencrypt({ renewal: ['example.org'], live: { 'example.org': CERT_PEM } });
    assert.deepEqual(collectCertificates({ letsencryptDir: root, now: HEALTHY }), { findings: [], failures: [], checked: 1 });
});

test('collectCertificates returns a finding for a certificate near expiry', () => {
    const root = letsencrypt({ renewal: ['example.org'], live: { 'example.org': CERT_PEM } });
    const result = collectCertificates({ letsencryptDir: root, now: EXPIRING });
    assert.equal(result.checked, 1);
    assert.deepEqual(result.failures, []);
    assert.deepEqual(result.findings.map((f) => [f.name, f.expired, f.days]), [['example.org', false, 5]]);
});

test('collectCertificates: a renewal config with no live certificate is a failure', () => {
    const root = letsencrypt({ renewal: ['example.org', 'example.net'], live: { 'example.org': CERT_PEM } });
    const result = collectCertificates({ letsencryptDir: root, now: HEALTHY });
    assert.equal(result.checked, 1);
    assert.equal(result.failures.length, 1);
    assert.match(result.failures[0], /^Could not read the TLS certificate `example\.net`: /);
});

test('collectCertificates: a missing live directory fails every renewal config', () => {
    const root = letsencrypt({ renewal: ['example.org'] });
    const result = collectCertificates({ letsencryptDir: root, now: HEALTHY });
    assert.equal(result.checked, 0);
    assert.match(result.failures.join('\n'), /Could not read the TLS certificate `example\.org`/);
});

test('collectCertificates: a live directory with no cert.pem, or a bad one, is a failure', () => {
    const root = letsencrypt({ live: { 'example.org': null, 'example.net': 'not a certificate' } });
    const result = collectCertificates({ letsencryptDir: root, now: HEALTHY });
    assert.equal(result.checked, 0);
    assert.deepEqual(result.failures.map((f) => f.match(/`([^`]+)`/)[1]), ['example.net', 'example.org']);
});

test('collectCertificates: directories that exist but hold no certificates are a failure', () => {
    const root = letsencrypt({ renewal: [], live: {} });
    const result = collectCertificates({ letsencryptDir: root, now: HEALTHY });
    assert.equal(result.checked, 0);
    assert.match(result.failures.join('\n'), /holds no certificates, so none were checked/);
});

test('collectCertificates: an error other than "not found" listing a directory is a failure', () => {
    // A file where the live directory should be: listing it fails with
    // ENOTDIR, which stands in for any error that is not ENOENT (e.g. EACCES).
    const root = letsencrypt({ renewal: ['example.org'] });
    fs.writeFileSync(path.join(root, 'live'), '');
    const result = collectCertificates({ letsencryptDir: root, now: HEALTHY });
    assert.match(result.failures.join('\n'), /Could not list the TLS certificates in .*live/);
    assert.match(result.failures.join('\n'), /Could not read the TLS certificate `example\.org`/);
});
