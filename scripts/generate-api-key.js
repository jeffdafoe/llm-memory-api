#!/usr/bin/env node
// Generate an API key for an agent and insert it into agent_api_keys.
// Usage: node scripts/generate-api-key.js <agent> [label]
// Must be run from the app root with env vars loaded.

const pool = require('../src/db');
const { hash, generateSalt, generateKey } = require('../src/services/hashing');

async function run() {
    const agent = process.argv[2];
    const label = process.argv[3] || 'default';

    if (!agent) {
        console.error('Usage: node scripts/generate-api-key.js <agent> [label]');
        process.exit(1);
    }

    // Verify agent exists
    const agentResult = await pool.query(
        'SELECT agent, status FROM agents WHERE agent = $1',
        [agent]
    );
    if (agentResult.rows.length === 0) {
        console.error(`Agent "${agent}" not found`);
        process.exit(1);
    }

    // Generate key
    const apiKey = generateKey();
    const salt = generateSalt();
    const keyHash = hash(apiKey, salt);

    await pool.query(
        'INSERT INTO agent_api_keys (agent, key_hash, key_salt, label) VALUES ($1, $2, $3, $4)',
        [agent, keyHash, salt, label]
    );

    // Grant all existing permissions (if not already granted)
    await pool.query(
        'INSERT INTO agent_permissions (agent, permission_id) SELECT $1, id FROM permissions ON CONFLICT DO NOTHING',
        [agent]
    );

    const permissions = await pool.query(
        'SELECT p.name FROM agent_permissions ap JOIN permissions p ON p.id = ap.permission_id WHERE ap.agent = $1',
        [agent]
    );

    console.log('Agent:', agent);
    console.log('Label:', label);
    console.log('API Key:', apiKey);
    console.log('Permissions:', permissions.rows.map(r => r.name).join(', '));
    console.log('\nUse this key as the OAuth client_secret. The agent name is the client_id.');

    pool.end();
}

run().catch(e => {
    console.error(e);
    pool.end();
    process.exit(1);
});
