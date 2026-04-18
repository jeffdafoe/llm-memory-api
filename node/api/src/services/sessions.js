// Shared session token validation — resolves a bearer token to an actor
// by iterating hashed sessions of the given kind. Used by auth middleware
// and WebSocket auth to avoid duplicating the hash-check loop.

const pool = require('../db');
const { verify } = require('./hashing');
const { SESSION_KIND } = require('../constants');

// Sliding session expiry — active sessions get pushed back to 24h from now
// whenever they fall within SLIDING_THRESHOLD of expiring. Bounds DB writes
// to at most once per SLIDING_THRESHOLD per active session.
const SESSION_TTL_MS = 24 * 60 * 60 * 1000;
const SLIDING_THRESHOLD_MS = 12 * 60 * 60 * 1000;

// Validate a session token against the sessions table.
// Returns the matching row ({ id, actor_id, name, expires_at }) or null.
// Extends expires_at when within SLIDING_THRESHOLD of expiry.
async function validateSessionToken(token, kind) {
    const result = await pool.query(
        `SELECT s.id, s.actor_id, s.token_hash, s.token_salt, s.expires_at, ac.name, ac.realms
         FROM sessions s
         JOIN actors ac ON ac.id = s.actor_id
         WHERE s.kind = $1 AND s.expires_at > NOW()`,
        [kind]
    );

    for (const row of result.rows) {
        if (verify(token, row.token_salt, row.token_hash)) {
            await maybeExtendExpiry(row);
            return row;
        }
    }

    return null;
}

// If the session's expires_at is within SLIDING_THRESHOLD of now, push it
// out to NOW() + SESSION_TTL. Writes directly rather than returning so
// callers see the updated expires_at on the row.
async function maybeExtendExpiry(row) {
    const expiresAtMs = new Date(row.expires_at).getTime();
    if (expiresAtMs > Date.now() + SLIDING_THRESHOLD_MS) {
        return;
    }
    const newExpiry = new Date(Date.now() + SESSION_TTL_MS);
    await pool.query(
        'UPDATE sessions SET expires_at = $1 WHERE id = $2',
        [newExpiry, row.id]
    );
    row.expires_at = newExpiry;
}

module.exports = { validateSessionToken };
