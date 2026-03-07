-- Restore previous agent_status view (from MEM-028)
CREATE OR REPLACE VIEW agent_status AS
SELECT agent,
       CASE
           WHEN last_seen > NOW() - INTERVAL '15 minutes' THEN 'online'
           WHEN last_seen IS NOT NULL THEN 'offline'
           ELSE 'unknown'
       END AS status,
       last_seen,
       passphrase_rotated_at,
       registered_at,
       expertise,
       provider,
       model
FROM agents;

ALTER TABLE agents DROP COLUMN IF EXISTS virtual;
ALTER TABLE agents DROP COLUMN IF EXISTS personality;
ALTER TABLE agents DROP COLUMN IF EXISTS api_key;
ALTER TABLE agents DROP COLUMN IF EXISTS configuration;
ALTER TABLE agents DROP COLUMN IF EXISTS cost;

DELETE FROM config WHERE key = 'virtual_agent_encryption_key';
