// Tests for requireSalemEngine — the service-identity gate on the engine-internal
// /sim routes (soul + conversation-day), added in LLM-241. Run with: node --test
// (from node/api). Uses the built-in node:test runner + node:assert, matching
// sim-soul.test.js / sim-conversation-distiller.test.js — no test-framework dep.
//
// There's no route/supertest harness in this repo, so we exercise the middleware
// function directly with a minimal req/res/next triple. The gate is a pure
// synchronous predicate over req.authenticatedAgent, so this fully covers it: the
// only legitimate caller is the salem-engine service account; every other
// authenticated actor (normal agent, admin/user web session) must be rejected.

const { test } = require('node:test');
const assert = require('node:assert/strict');
const { requireSalemEngine } = require('./sim');

// Minimal res double: captures the status code and JSON body, and records
// whether a response was sent. res.status(n) returns res so the route's
// `res.status(403).json(...)` chain works.
function makeRes() {
    return {
        statusCode: null,
        body: null,
        sent: false,
        status(code) {
            this.statusCode = code;
            return this;
        },
        json(payload) {
            this.body = payload;
            this.sent = true;
            return this;
        },
    };
}

test('allows the salem-engine service account through', () => {
    const req = { authenticatedAgent: 'salem-engine' };
    const res = makeRes();
    let nextCalled = false;

    requireSalemEngine(req, res, () => { nextCalled = true; });

    assert.equal(nextCalled, true, 'next() should be called for salem-engine');
    assert.equal(res.sent, false, 'no response should be sent on the allow path');
});

test('rejects a normal authenticated agent with 403', () => {
    const req = { authenticatedAgent: 'work' };
    const res = makeRes();
    let nextCalled = false;

    requireSalemEngine(req, res, () => { nextCalled = true; });

    assert.equal(nextCalled, false, 'next() must not be called for a non-engine agent');
    assert.equal(res.statusCode, 403);
    assert.equal(res.body.error.code, 'FORBIDDEN');
});

test('rejects an admin/user web session (no authenticatedAgent) with 403', () => {
    // Web/admin sessions set req.authenticatedUser and leave authenticatedAgent
    // undefined — these routes are engine-internal, not part of the admin surface.
    const req = { authenticatedUser: { id: 1, username: 'jeff' } };
    const res = makeRes();
    let nextCalled = false;

    requireSalemEngine(req, res, () => { nextCalled = true; });

    assert.equal(nextCalled, false, 'next() must not be called for an admin/user session');
    assert.equal(res.statusCode, 403);
    assert.equal(res.body.error.code, 'FORBIDDEN');
});

test('rejects an unauthenticated request (no principal at all) with 403', () => {
    const req = {};
    const res = makeRes();
    let nextCalled = false;

    requireSalemEngine(req, res, () => { nextCalled = true; });

    assert.equal(nextCalled, false);
    assert.equal(res.statusCode, 403);
});
