// Shared session token validation — resolves a bearer token to an actor
// by iterating hashed sessions of the given kind. Used by auth middleware
// and WebSocket auth to avoid duplicating the hash-check loop.

const pool = require('../db');
const { verify } = require('./hashing');
const { SESSION_KIND } = require('../constants');

// Validate a session token against the sessions table.
// Returns the matching row ({ id, actor_id, name, expires_at }) or null.
async function validateSessionToken(token, kind) {
    const result = await pool.query(
        `SELECT s.id, s.actor_id, s.token_hash, s.token_salt, s.expires_at, ac.name
         FROM sessions s
         JOIN actors ac ON ac.id = s.actor_id
         WHERE s.kind = $1 AND s.expires_at > NOW()`,
        [kind]
    );

    for (const row of result.rows) {
        if (verify(token, row.token_salt, row.token_hash)) {
            return row;
        }
    }

    return null;
}

module.exports = { validateSessionToken };
