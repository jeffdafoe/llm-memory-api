-- MEM-039: Agent activity indicator
-- Adds active_since column so agents can signal when they're busy working.
-- NULL = idle, non-NULL = actively working (timestamp for stale detection).

ALTER TABLE agents ADD COLUMN active_since TIMESTAMPTZ DEFAULT NULL;

-- Update agent_status view to include active_since
CREATE OR REPLACE VIEW agent_status AS
SELECT agent,
       CASE
           WHEN virtual = TRUE THEN 'online'
           WHEN last_seen > NOW() - INTERVAL '15 minutes' THEN 'online'
           WHEN last_seen IS NOT NULL THEN 'offline'
           ELSE 'unknown'
       END AS status,
       last_seen,
       passphrase_rotated_at,
       registered_at,
       expertise,
       provider,
       model,
       virtual,
       personality,
       cost,
       CASE
           WHEN active_since IS NOT NULL AND active_since > NOW() - INTERVAL '30 minutes' THEN active_since
           ELSE NULL
       END AS active_since
FROM agents;
