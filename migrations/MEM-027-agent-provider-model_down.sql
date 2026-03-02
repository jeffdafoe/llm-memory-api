-- MEM-027 rollback: Remove provider and model columns from agents table.

-- Restore the original view without provider/model
CREATE OR REPLACE VIEW agent_status AS
SELECT agent,
       CASE
           WHEN last_seen > NOW() - INTERVAL '5 minutes' THEN 'online'
           WHEN last_seen IS NOT NULL THEN 'offline'
           ELSE 'unknown'
       END AS status,
       last_seen,
       passphrase_rotated_at,
       registered_at
FROM agents;

ALTER TABLE agents DROP COLUMN IF EXISTS provider;
ALTER TABLE agents DROP COLUMN IF EXISTS model;
