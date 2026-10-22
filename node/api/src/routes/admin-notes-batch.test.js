// Route tests for the batched Memories load (LLM-829): POST /admin/notes/list
// with a `namespaces` array, and POST /admin/shares/list with `all: true`.
// Run with: node --test (from node/api).
//
// The admin router is mounted on a throwaway Express app. A stand-in for the
// auth middleware sets the caller from the x-test-actor header. The db pool
// answers the two permission lookups (admin_permissions for requirePerm,
// namespace_permissions for requireAccess / getReadableNamespaces), so those
// checks run for real. listNotes and listAllShares are stubbed; listNotes must
// be stubbed before admin.js is required because admin.js destructures it.

const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const express = require('express');

const pool = require('../db');
const documents = require('../services/documents');
const notePerms = require('../services/note-permissions');

// Two callers: wendy can read every namespace ('/'), carol can read only
// "alpha" (plus her own "carol", which every actor can read implicitly).
const ACTORS = {
    1: { username: 'wendy', namespaces: [{ namespace: '/', can_read: true, can_write: true, can_delete: true }] },
    2: { username: 'carol', namespaces: [{ namespace: 'alpha', can_read: true, can_write: false, can_delete: false }] }
};

pool.query = async (sql, params) => {
    if (/FROM admin_permissions/.test(sql)) {
        return { rows: [{ resource: 'notes', action: 'read' }] };
    }
    if (/FROM namespace_permissions/.test(sql)) {
        const actor = ACTORS[params[0]];
        return { rows: actor ? actor.namespaces : [] };
    }
    return { rows: [] };
};

let listNotesCalls = [];
documents.listNotes = async (namespace, limit) => {
    listNotesCalls.push({ namespace, limit });
    return { notes: [{ slug: `${namespace}/first` }], total: 1 };
};

notePerms.listAllShares = async () => [
    { owner_namespace: 'alpha', slug_pattern: 'a/' },
    { owner_namespace: 'beta', slug_pattern: 'b' },
    { owner_namespace: 'carol', slug_pattern: 'c' }
];

const adminRouter = require('./admin');

let server;
let base;

before(async () => {
    const app = express();
    app.use(express.json());
    app.use((req, res, next) => {
        const actor = ACTORS[req.headers['x-test-actor']];
        if (actor) {
            req.actorId = Number(req.headers['x-test-actor']);
            req.authenticatedUser = { id: req.actorId, username: actor.username };
        }
        next();
    });
    app.use(adminRouter);
    await new Promise((resolve) => {
        server = app.listen(0, '127.0.0.1', resolve);
    });
    base = `http://127.0.0.1:${server.address().port}`;
});

after(() => {
    server.close();
});

async function post(path, actorId, body) {
    const response = await fetch(`${base}${path}`, {
        method: 'POST',
        headers: { 'content-type': 'application/json', 'x-test-actor': String(actorId) },
        body: JSON.stringify(body)
    });
    return { status: response.status, body: await response.json() };
}

test('a batched list returns every namespace, in the order asked', async () => {
    listNotesCalls = [];
    const { status, body } = await post('/admin/notes/list', 1, { namespaces: ['beta', 'alpha'], limit: 500 });
    assert.equal(status, 200);
    assert.deepEqual(body.results.map(r => r.namespace), ['beta', 'alpha']);
    assert.deepEqual(body.results[0].notes, [{ slug: 'beta/first' }]);
    assert.deepEqual(listNotesCalls, [{ namespace: 'beta', limit: 500 }, { namespace: 'alpha', limit: 500 }]);
});

test('a limited user can batch the namespaces they can read, including their own', async () => {
    const { status, body } = await post('/admin/notes/list', 2, { namespaces: ['alpha', 'carol'] });
    assert.equal(status, 200);
    assert.deepEqual(body.results.map(r => r.namespace), ['alpha', 'carol']);
});

test('one unreadable namespace fails the whole batch before anything is read', async () => {
    listNotesCalls = [];
    const { status } = await post('/admin/notes/list', 2, { namespaces: ['alpha', 'beta'] });
    assert.equal(status, 403);
    assert.deepEqual(listNotesCalls, [], 'no namespace is listed when one is refused');
});

test('a malformed or oversized batch is a 400', async () => {
    for (const namespaces of ['alpha', [''], ['alpha', 7], Array.from({ length: 501 }, (_, i) => `ns${i}`)]) {
        const { status } = await post('/admin/notes/list', 1, { namespaces });
        assert.equal(status, 400, JSON.stringify(namespaces).slice(0, 40));
    }
});

test('a reserved namespace in a batch is refused', async () => {
    const { status } = await post('/admin/notes/list', 1, { namespaces: ['alpha', '*'] });
    assert.equal(status, 400);
});

test('the single-namespace form is unchanged', async () => {
    const { status, body } = await post('/admin/notes/list', 2, { namespace: 'alpha' });
    assert.equal(status, 200);
    assert.deepEqual(body.notes, [{ slug: 'alpha/first' }]);
    const refused = await post('/admin/notes/list', 2, { namespace: 'beta' });
    assert.equal(refused.status, 403);
});

test('all shares: a wildcard reader sees every namespace', async () => {
    const { status, body } = await post('/admin/shares/list', 1, { all: true });
    assert.equal(status, 200);
    assert.deepEqual(body.shares.map(s => s.owner_namespace), ['alpha', 'beta', 'carol']);
});

test('all shares: a limited user sees only namespaces they can read', async () => {
    const { status, body } = await post('/admin/shares/list', 2, { all: true });
    assert.equal(status, 200);
    assert.deepEqual(body.shares.map(s => s.owner_namespace), ['alpha', 'carol']);
});
