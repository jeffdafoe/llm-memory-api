// Route test for POST /admin/login (LLM-733 moved its password check into
// services/password-login.js, shared with the claude.ai connector login).
// Run with: node --test (from node/api).
//
// The admin router is mounted on a throwaway Express app with the db pool and
// the permission lookup stubbed before admin.js is required. Passwords are
// hashed for real.

const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const express = require('express');

const pool = require('../db');
const adminPermissions = require('../services/admin-permissions');
const { hash, generateSalt } = require('../services/hashing');

// name -> actors row; a name missing here has no row WITH a password, which is
// how the query reads both "no such account" and "account without a password".
const accounts = {};
let sessionInserts = [];

adminPermissions.getPermissionMap = async () => ({ dashboard: ['read'] });
pool.query = async (sql, params) => {
    if (/password_hash IS NOT NULL/.test(sql)) {
        const row = accounts[params[0]];
        return { rows: row ? [row] : [] };
    }
    if (/INSERT INTO sessions/.test(sql)) {
        sessionInserts.push(params);
        return { rows: [] };
    }
    return { rows: [] };
};

const adminRouter = require('./admin');

let server;
let base;

before(async () => {
    const salt = generateSalt();
    accounts.alice = { id: 7, name: 'alice', password_salt: salt, password_hash: await hash('right password', salt) };
    const app = express();
    app.use(express.json());
    app.use(adminRouter);
    await new Promise((resolve) => {
        server = app.listen(0, '127.0.0.1', resolve);
    });
    base = `http://127.0.0.1:${server.address().port}`;
});

after(() => {
    server.close();
});

async function loginAs(username, password) {
    return fetch(`${base}/admin/login`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ username, password })
    });
}

test('the right password creates a web session for that account', async () => {
    sessionInserts = [];
    const response = await loginAs('Alice', 'right password');
    assert.equal(response.status, 200);
    const body = await response.json();
    assert.ok(body.session_token);
    assert.deepEqual(body.user, { id: 7, username: 'alice' });
    assert.deepEqual(body.permissions, { dashboard: ['read'] });
    assert.equal(sessionInserts.length, 1);
    assert.equal(sessionInserts[0][0], 7, 'the session belongs to the account that logged in');
});

test('a wrong password, an unknown name and a passwordless account get the same 401', async () => {
    sessionInserts = [];
    for (const [username, password] of [['alice', 'wrong password'], ['nobody', 'right password'], ['bot-without-password', 'x']]) {
        const response = await loginAs(username, password);
        assert.equal(response.status, 401, username);
        const body = await response.json();
        assert.equal(body.error.code, 'INVALID_CREDENTIALS', username);
        assert.equal(body.error.message, 'Invalid username or password', username);
    }
    assert.equal(sessionInserts.length, 0);
});

test('missing fields are a 400', async () => {
    const response = await loginAs('alice', '');
    assert.equal(response.status, 400);
});
