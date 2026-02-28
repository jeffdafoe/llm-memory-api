-- MEM-019: Create a view that computes agent online/offline status from last_seen.
-- Centralizes the 5-minute threshold so all queries use the same logic.

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
