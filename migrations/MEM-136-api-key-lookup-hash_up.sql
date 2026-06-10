-- MEM-136 — indexed lookup hash for API keys / client secrets (ZBBS-HOME-367).
--
-- Symptom: every API-key auth (middleware/auth.js, middleware/mcp-auth.js)
-- and OAuth client-secret check (routes/oauth.js) PBKDF2-verifies the
-- presented token against EVERY non-revoked row of agent_api_keys until one
-- matches — the same O(N * ~50ms) shape MEM-131 removed from sessions.
-- After ZBBS-HOME-366 made PBKDF2 async this no longer freezes the event
-- loop, but it still burns one libuv threadpool slot (default pool: 4) per
-- candidate row per auth, so a growing key fleet degrades all auth latency.
--
-- Fix shape (MEM-131 precedent): a deterministic SHA-256 lookup column.
-- New key inserts populate it; auth does an indexed single-candidate SELECT
-- then ONE PBKDF2 verify. Backfill is impossible — only the PBKDF2 hash is
-- stored, the key itself is unrecoverable — so pre-migration rows keep NULL
-- and authenticate via a bounded legacy scan (key_lookup_hash IS NULL AND
-- revoked_at IS NULL). Unlike sessions (24h expiry), API keys live forever,
-- so the legacy path SELF-HEALS instead of waiting out an expiry: a
-- successful legacy verify has the plaintext in hand and stamps
-- key_lookup_hash on its row, moving that key to the indexed path from the
-- next auth onward (services/api-keys.js).
--
-- Column nullable so existing rows don't break; index partial to skip the
-- all-NULL pre-migration fleet.

ALTER TABLE agent_api_keys
    ADD COLUMN key_lookup_hash TEXT NULL;

CREATE INDEX idx_agent_api_keys_key_lookup_hash
    ON agent_api_keys (key_lookup_hash)
 WHERE key_lookup_hash IS NOT NULL;
