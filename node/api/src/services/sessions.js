// Shared session token validation — resolves a bearer token to an actor
// by looking up the session row keyed on the token's deterministic
// SHA-256 hash. Used by auth middleware, /v1/auth/verify, and WebSocket
// auth to avoid duplicating the verification logic.

const pool = require('../db');
const { verify, tokenLookupHash } = require('./hashing');
const { SESSION_KIND } = require('../constants');

// Sliding session expiry — active sessions get pushed back to 24h from now
// whenever they fall within SLIDING_THRESHOLD of expiring. Bounds DB writes
// to at most once per SLIDING_THRESHOLD per active session.
const SESSION_TTL_MS = 24 * 60 * 60 * 1000;
const SLIDING_THRESHOLD_MS = 12 * 60 * 60 * 1000;

// Validate a session token against the sessions table.
//
// Fast path (post-MEM-131): SELECT WHERE token_lookup_hash = sha256(token)
// returns at most one candidate row, then PBKDF2-verify confirms. O(1)
// indexed lookup + 1 PBKDF2 call.
//
// Fallback (legacy): for sessions inserted before MEM-131 (token_lookup_hash
// IS NULL), iterate all rows of the kind and PBKDF2-verify each. The
// fallback is bounded by the count of pre-migration sessions still alive;
// after 24h every legacy session has expired and the fallback can be
// removed in a follow-up.
//
// Returns the matching row ({ id, actor_id, name, expires_at, ... }) or
// null. Extends expires_at when within SLIDING_THRESHOLD of expiry.
async function validateSessionToken(token, kind) {
    const lookupHash = tokenLookupHash(token);
    const fast = await pool.query(
        `SELECT s.id, s.actor_id, s.token_hash, s.token_salt, s.expires_at, ac.name, ac.realms
         FROM sessions s
         JOIN actors ac ON ac.id = s.actor_id
         WHERE s.kind = $1
           AND s.token_lookup_hash = $2
           AND s.expires_at > NOW()
         LIMIT 1`,
        [kind, lookupHash]
    );
    if (fast.rows.length > 0) {
        const row = fast.rows[0];
        if (verify(token, row.token_salt, row.token_hash)) {
            await maybeExtendExpiry(row);
            return row;
        }
        // Lookup-hash collision (vanishingly unlikely with 256-bit SHA)
        // or a token that hashed the same prefix but differs — fail
        // closed rather than fall through to the legacy scan.
        return null;
    }

    // Legacy fallback for sessions inserted before MEM-131.
    const legacy = await pool.query(
        `SELECT s.id, s.actor_id, s.token_hash, s.token_salt, s.expires_at, ac.name, ac.realms
         FROM sessions s
         JOIN actors ac ON ac.id = s.actor_id
         WHERE s.kind = $1
           AND s.token_lookup_hash IS NULL
           AND s.expires_at > NOW()`,
        [kind]
    );
    for (const row of legacy.rows) {
        if (verify(token, row.token_salt, row.token_hash)) {
            await maybeExtendExpiry(row);
            return row;
        }
    }
    return null;
}

// Find and delete the single session matching this bearer token, scoped to
// one actor and session kind. Mirrors validateSessionToken's indexed lookup:
// SELECT WHERE token_lookup_hash = sha256(token) returns at most one candidate,
// then a single PBKDF2 verify confirms it — O(1) instead of PBKDF2-verifying
// every session row the actor owns.
//
// The unbounded scan this replaces was an event-loop DoS: PBKDF2 is a
// deliberately slow synchronous hash (~30ms each), so an actor that
// accumulated thousands of sessions (e.g. a client that logs in repeatedly
// without ever logging out) froze the single-threaded process for minutes on
// each logout, hanging every other request behind it.
//
// Returns true if a session was deleted, false if no session matched.
async function deleteSessionByToken(token, kind, actorId) {
    const lookupHash = tokenLookupHash(token);
    const fast = await pool.query(
        `SELECT id, token_hash, token_salt FROM sessions
         WHERE actor_id = $1 AND kind = $2 AND token_lookup_hash = $3
         LIMIT 1`,
        [actorId, kind, lookupHash]
    );
    if (fast.rows.length > 0) {
        const row = fast.rows[0];
        if (verify(token, row.token_salt, row.token_hash)) {
            await pool.query('DELETE FROM sessions WHERE id = $1', [row.id]);
            return true;
        }
        // Lookup-hash matched but PBKDF2 verify failed — fail closed rather
        // than fall through to the legacy scan.
        return false;
    }

    // Legacy fallback for sessions inserted before MEM-131 (token_lookup_hash
    // IS NULL). Bounded by the count of such rows still alive for this actor;
    // ~0 now that the 24h TTL has long elapsed since that migration.
    const legacy = await pool.query(
        `SELECT id, token_hash, token_salt FROM sessions
         WHERE actor_id = $1 AND kind = $2 AND token_lookup_hash IS NULL`,
        [actorId, kind]
    );
    for (const row of legacy.rows) {
        if (verify(token, row.token_salt, row.token_hash)) {
            await pool.query('DELETE FROM sessions WHERE id = $1', [row.id]);
            return true;
        }
    }
    return false;
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

module.exports = { validateSessionToken, deleteSessionByToken };
