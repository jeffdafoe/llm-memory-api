-- MEM-024 rollback: Remove expertise column

-- Recreate view without expertise
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

ALTER TABLE agents DROP COLUMN expertise;
