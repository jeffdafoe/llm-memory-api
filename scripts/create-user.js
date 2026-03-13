#!/usr/bin/env node
// Create an admin user for the dashboard.
// Usage: node scripts/create-user.js <username> <password>
//
// If the actor already exists, updates the password only if the actor
// already has web credentials (password_hash set). This prevents
// accidentally overwriting an agent-only actor's identity.

const { Pool } = require('pg');
const { hash, generateSalt } = require('../node/api/src/services/hashing');

const [username, password] = process.argv.slice(2);

if (!username || !password) {
    console.error('Usage: node scripts/create-user.js <username> <password>');
    process.exit(1);
}

const pool = new Pool({ connectionString: process.env.DATABASE_URL });

(async () => {
    try {
        const salt = generateSalt();
        const passwordHash = hash(password, salt);

        // Check if actor already exists
        const existing = await pool.query('SELECT id, password_hash FROM actors WHERE name = $1', [username]);

        if (existing.rows.length > 0) {
            const actor = existing.rows[0];
            if (actor.password_hash === null) {
                // Actor exists but has no web credentials (agent-only).
                // Refuse to silently convert — this would grant dashboard access.
                console.error(`Error: Actor "${username}" exists as an agent without web credentials.`);
                console.error('Refusing to add web credentials. To grant dual identity, use the admin UI.');
                process.exit(1);
            }
            await pool.query(
                'UPDATE actors SET password_hash = $1, password_salt = $2 WHERE id = $3',
                [passwordHash, salt, actor.id]
            );
            console.log(`User "${username}" password updated.`);
        } else {
            // Create new actor with web credentials only
            await pool.query(
                'INSERT INTO actors (name, password_hash, password_salt) VALUES ($1, $2, $3)',
                [username, passwordHash, salt]
            );
            console.log(`User "${username}" created.`);
        }
    } catch (err) {
        console.error('Failed:', err.message);
        process.exit(1);
    } finally {
        await pool.end();
    }
})();
