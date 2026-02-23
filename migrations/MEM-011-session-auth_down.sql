DROP TABLE IF EXISTS agent_sessions;
ALTER TABLE agents DROP COLUMN IF EXISTS passphrase_rotated_at;
