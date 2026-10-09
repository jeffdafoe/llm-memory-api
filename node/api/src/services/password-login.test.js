// Tests for the shared agent-name + password check (LLM-733), used by the
// dashboard login and the claude.ai connector login. Run with: node --test
// (from node/api). The db pool is stubbed; hashing is real.

const { test, before } = require('node:test');
const assert = require('node:assert/strict');

const pool = require('../db');
const { hash, generateSalt } = require('./hashing');

let rows = {};
let queries = [];
pool.query = async (sql, params) => {
    queries.push({ sql, params });
    const row = rows[params[0]];
    return { rows: row ? [row] : [] };
};

const { verifyPasswordLogin } = require('./password-login');

before(async () => {
    const salt = generateSalt();
    rows = { alice: { id: 7, name: 'alice', password_salt: salt, password_hash: await hash('right password', salt) } };
});

test('the right password returns the account', async () => {
    assert.deepEqual(await verifyPasswordLogin('alice', 'right password'), { id: 7, name: 'alice' });
});

test('a wrong password returns null', async () => {
    assert.equal(await verifyPasswordLogin('alice', 'wrong password'), null);
});

test('an unknown name returns null, and only accounts with a password are looked up', async () => {
    queries = [];
    assert.equal(await verifyPasswordLogin('nobody', 'right password'), null);
    assert.match(queries[0].sql, /password_hash IS NOT NULL/);
});
