-- MEM-136 reverse: drop the API-key lookup-hash column + partial index.
-- Safe at any time — the auth code's legacy fallback authenticates rows
-- without a lookup hash, and pre-MEM-136 code never references the column.

DROP INDEX IF EXISTS idx_agent_api_keys_key_lookup_hash;

ALTER TABLE agent_api_keys
    DROP COLUMN IF EXISTS key_lookup_hash;
