// Tests for the admin Content-Security-Policy (LLM-27). Run with: node --test
// (from node/api). Uses node:test + node:assert, matching the route tests.
//
// The middleware is mounted the way server.js mounts it — ahead of
// express.static on /admin — on a throwaway Express app serving a temp
// directory, so the test does not need a built admin bundle.

const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const express = require('express');
const fs = require('fs');
const os = require('os');
const path = require('path');

const { adminCsp, ADMIN_CSP } = require('./admin-csp');

const ADMIN_SRC = path.join(__dirname, '..', '..', 'public', 'admin');

let server;
let base;
let staticDir;

before(async () => {
    staticDir = fs.mkdtempSync(path.join(os.tmpdir(), 'admin-csp-'));
    fs.writeFileSync(path.join(staticDir, 'index.html'), '<div id="app"></div>');
    const app = express();
    app.use('/admin', adminCsp);
    app.use('/admin', express.static(staticDir));
    app.get('/other', (req, res) => res.send('ok'));
    await new Promise((resolve) => {
        server = app.listen(0, '127.0.0.1', resolve);
    });
    base = `http://127.0.0.1:${server.address().port}`;
});

after(() => {
    server.close();
    fs.rmSync(staticDir, { recursive: true, force: true });
});

test('the admin page carries the CSP', async () => {
    const res = await fetch(`${base}/admin/`);
    assert.equal(res.status, 200);
    assert.equal(res.headers.get('content-security-policy'), ADMIN_CSP);
});

test('the policy allows only same-origin script files', () => {
    const directives = ADMIN_CSP.split(';').map((d) => d.trim());
    const scriptSrc = directives.find((d) => d.startsWith('script-src'));
    assert.equal(scriptSrc, "script-src 'self'");
    assert.ok(directives.includes("object-src 'none'"));
    assert.ok(directives.includes("base-uri 'none'"));
    assert.ok(directives.includes("form-action 'self'"));
    assert.ok(directives.includes("frame-ancestors 'none'"));
    assert.ok(!ADMIN_CSP.includes('unsafe-'), 'no unsafe-inline / unsafe-eval');
});

test('routes outside /admin are not affected', async () => {
    const res = await fetch(`${base}/other`);
    assert.equal(res.status, 200);
    assert.equal(res.headers.get('content-security-policy'), null);
});

// script-src 'self' holds only while no admin component is compiled in the
// browser. A component that declares a `template` option (a string or a
// ?raw import) needs Vue's runtime compiler, which uses Function() and would
// be blocked: the view would render blank. Templates belong in a .html file
// imported with ?render (vite.config.js compiles those at build time).
test('no admin component declares a runtime template', () => {
    const offenders = [];
    const walk = (dir) => {
        for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
            const full = path.join(dir, entry.name);
            if (entry.isDirectory()) {
                if (entry.name !== 'dist') {
                    walk(full);
                }
            } else if (entry.name.endsWith('.js')) {
                const src = fs.readFileSync(full, 'utf8');
                if (/^\s*template\s*[,:]/m.test(src) || src.includes('.html?raw')) {
                    offenders.push(path.relative(ADMIN_SRC, full));
                }
            }
        }
    };
    walk(ADMIN_SRC);
    assert.deepEqual(offenders, []);
});
