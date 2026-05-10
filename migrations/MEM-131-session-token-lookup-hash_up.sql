-- MEM-131 — fast path for /v1/auth/verify and middleware session lookup.
--
-- Symptom: validateSessionToken (services/sessions.js) iterates every
-- non-expired session of the given kind and calls PBKDF2(token, salt)
-- against each row until one matches. PBKDF2 100k iterations is ~50-100ms;
-- with N active sessions every verify call burns up to N*100ms. Measured
-- on the VPS: 770ms baseline / 2.4s spikes, which intermittently exceed
-- the salem-engine's 5s `/v1/auth/verify` timeout and 503 page-load calls.
--
-- Fix shape: add a deterministic SHA-256 lookup column on the sessions
-- table. New session inserts populate it. Validation does an indexed
-- single-row lookup and falls back to the O(N) PBKDF2 loop only when the
-- new column is null (i.e. for sessions issued before this migration).
-- 24h after deploy all such sessions have expired and the fallback path
-- can be dropped in a follow-up.
--
-- The column is nullable so existing rows don't break; the index is
-- partial (WHERE token_lookup_hash IS NOT NULL) to skip the all-NULL
-- pre-migration fleet.

ALTER TABLE sessions
    ADD COLUMN token_lookup_hash TEXT NULL;

CREATE INDEX idx_sessions_token_lookup_hash
    ON sessions (token_lookup_hash)
 WHERE token_lookup_hash IS NOT NULL;
