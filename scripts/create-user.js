#!/usr/bin/env node
// Create an admin user for the dashboard.
// Usage: node scripts/create-user.js <username> <password>

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

        await pool.query(
            `INSERT INTO users (username, password_hash, password_salt) VALUES ($1, $2, $3)
             ON CONFLICT (username) DO UPDATE SET password_hash = $2, password_salt = $3`,
            [username, passwordHash, salt]
        );

        console.log(`User "${username}" created (or password updated).`);
    } catch (err) {
        console.error('Failed:', err.message);
        process.exit(1);
    } finally {
        await pool.end();
    }
})();
