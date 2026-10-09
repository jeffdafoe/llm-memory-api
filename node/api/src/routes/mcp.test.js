// Tests for the MCP save_note duplicate-slug message (LLM-729). Run with:
// node --test (from node/api). Uses node:test + node:assert, matching
// agent.test.js.
//
// The real documents.saveNote runs; only the db pool and a few lookups are
// stubbed, so the test covers the service's DUPLICATE_SLUG error and the MCP
// handler's rewrite of it together. Services destructure their imports at
// require time, so every stub is set before mcp.js is required.

const { test } = require('node:test');
const assert = require('node:assert/strict');

const pool = require('../db');
const actors = require('../services/actors');
const config = require('../services/config');
const namespacePermissions = require('../services/namespace-permissions');

// The INSERT fails the way PostgreSQL reports a unique violation; every other
// query (quota lookups) finds nothing.
pool.query = async (sql) => {
    if (/INSERT INTO documents/.test(sql)) {
        throw Object.assign(new Error('duplicate key value'), { code: '23505' });
    }
    return { rows: [] };
};
actors.resolveByName = async () => ({ id: 1 });
config.get = (key) => {
    if (key === 'default_storage_quota') {
        throw new Error('not configured');
    }
    return undefined;
};
namespacePermissions.hasAccess = async () => true;

const { TOOL_HANDLERS } = require('./mcp');
const { saveNote } = require('../services/documents');

test('MCP save_note on an existing slug points at edit_note, not upsert', async () => {
    await assert.rejects(
        TOOL_HANDLERS.save_note(
            { slug: 'notes/active-work', title: 'Active Work', content: 'new text' },
            'remi', 'remi', 1
        ),
        (err) => {
            assert.equal(err.code, 'DUPLICATE_SLUG');
            assert.equal(err.statusCode, 409);
            assert.match(err.message, /"notes\/active-work" in namespace "remi"/);
            assert.match(err.message, /use edit_note/);
            assert.doesNotMatch(err.message, /upsert/);
            return true;
        }
    );
});

test('the service message still offers upsert to REST callers', async () => {
    await assert.rejects(
        saveNote('remi', 'Active Work', 'new text', 'notes/active-work', 'remi'),
        (err) => {
            assert.equal(err.code, 'DUPLICATE_SLUG');
            assert.match(err.message, /pass upsert:true/);
            return true;
        }
    );
});
