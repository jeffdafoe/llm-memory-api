// Shared API-key lookup — resolves a presented key (bearer token or OAuth
// client_secret) to its agent_api_keys row via the indexed key_lookup_hash
// (MEM-136), mirroring services/sessions.js's MEM-131 pattern. Used by the
// /v1 auth middleware, the /mcp auth middleware, and the OAuth token
// endpoint so all three share one verification path instead of each
// carrying its own PBKDF2 scan loop.
//
// Fast path: SELECT WHERE key_lookup_hash = sha256(key) returns at most one
// candidate row, then ONE PBKDF2 verify confirms. O(1) indexed lookup.
//
// Legacy fallback: keys issued before MEM-136 have key_lookup_hash NULL and
// are PBKDF2-verified row by row, bounded to revoked_at IS NULL. Unlike the
// sessions fallback (which aged out with the 24h expiry), API keys never
// expire — so a successful legacy verify SELF-HEALS: with the plaintext in
// hand it stamps key_lookup_hash onto the row, moving that key to the
// indexed path for every subsequent auth. The fallback population only
// shrinks (heal, rotation, or revocation) and never grows, since all new
// keys insert with the hash populated (routes/registration.js).

const pool = require('../db');
const { verify, tokenLookupHash } = require('./hashing');

// Resolve a presented key to { id, actorId, agent } or null.
//
// options.actorId scopes the search to one actor's keys (the OAuth
// client-credentials case, where client_id names the actor and the secret
// must belong to it). Without it, the key alone identifies the principal
// (the bearer-token middlewares).
//
// Side effect on success: fire-and-forget last_used_at stamp (and, on a
// legacy hit, the self-heal key_lookup_hash stamp in the same UPDATE).
// Failures of these updates are swallowed — they're telemetry/optimization,
// not authorization, and the next successful auth retries them.
async function findApiKeyByToken(token, options) {
    // Shape gate (code_review R1): every key generateKey() has ever issued
    // is 64 hex chars, so anything else can be rejected before hashing —
    // and, more importantly, before the legacy PBKDF2 fallback below.
    // Centralized here so all three callers get it (mcp-auth and oauth had
    // no local check; mcp-auth in particular used to PBKDF2-scan the table
    // for ANY non-HMAC bearer string).
    if (typeof token !== 'string' || !/^[0-9a-f]{64}$/i.test(token)) {
        return null;
    }
    let actorId = null;
    if (options && options.actorId) {
        actorId = options.actorId;
    }
    const lookupHash = tokenLookupHash(token);

    const fastParams = [lookupHash];
    let fastWhere = 'ak.key_lookup_hash = $1 AND ak.revoked_at IS NULL';
    if (actorId !== null) {
        fastParams.push(actorId);
        fastWhere += ` AND ak.actor_id = $${fastParams.length}`;
    }
    const fast = await pool.query(
        `SELECT ak.id, ak.actor_id, ak.key_hash, ak.key_salt, ac.name AS agent
         FROM agent_api_keys ak
         JOIN actors ac ON ac.id = ak.actor_id
         WHERE ${fastWhere}
         LIMIT 1`,
        fastParams
    );
    if (fast.rows.length > 0) {
        const row = fast.rows[0];
        if (await verify(token, row.key_salt, row.key_hash)) {
            pool.query(
                'UPDATE agent_api_keys SET last_used_at = NOW() WHERE id = $1',
                [row.id]
            ).catch(() => {});
            return { id: row.id, actorId: row.actor_id, agent: row.agent };
        }
        // Lookup-hash matched but PBKDF2 verify failed (256-bit collision,
        // vanishingly unlikely) — fail closed rather than fall through to
        // the legacy scan, same posture as validateSessionToken.
        return null;
    }

    // Legacy fallback for keys issued before MEM-136 (key_lookup_hash NULL).
    const legacyParams = [];
    let legacyWhere = 'ak.key_lookup_hash IS NULL AND ak.revoked_at IS NULL';
    if (actorId !== null) {
        legacyParams.push(actorId);
        legacyWhere += ` AND ak.actor_id = $${legacyParams.length}`;
    }
    const legacy = await pool.query(
        `SELECT ak.id, ak.actor_id, ak.key_hash, ak.key_salt, ac.name AS agent
         FROM agent_api_keys ak
         JOIN actors ac ON ac.id = ak.actor_id
         WHERE ${legacyWhere}`,
        legacyParams
    );
    // Residual scan visibility (code_review R1): a well-formed-but-invalid
    // key that misses the fast path PBKDF2-scans whatever legacy rows
    // remain — self-heal only shrinks that population via SUCCESSFUL auths,
    // so dormant pre-MEM-136 keys keep this path alive until rotated or
    // revoked. Log the scan so the remaining fleet is observable in prod;
    // the follow-up (drop the fallback once the NULL population is zero)
    // is tracked in shared/tasks/pending.
    if (legacy.rows.length > 0) {
        console.warn(`api-keys: legacy fallback scanning ${legacy.rows.length} pre-MEM-136 row(s) (self-heals on successful auth)`);
    }
    for (const row of legacy.rows) {
        if (await verify(token, row.key_salt, row.key_hash)) {
            // Self-heal: this key's plaintext is in hand exactly here, so
            // stamp its lookup hash — next auth takes the indexed path.
            pool.query(
                'UPDATE agent_api_keys SET key_lookup_hash = $1, last_used_at = NOW() WHERE id = $2',
                [lookupHash, row.id]
            ).catch(() => {});
            return { id: row.id, actorId: row.actor_id, agent: row.agent };
        }
    }
    return null;
}

module.exports = { findApiKeyByToken };
