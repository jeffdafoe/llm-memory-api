-- MEM-024: Add expertise column to agents table.
-- Stores a JSON array of strings (e.g. '["codebase","devops","payments"]').
-- TEXT column with JSON parsed in app code for DB portability.

ALTER TABLE agents ADD COLUMN expertise TEXT NOT NULL DEFAULT '[]';

-- Recreate agent_status view to include the new column
CREATE OR REPLACE VIEW agent_status AS
SELECT agent,
       CASE
           WHEN last_seen > NOW() - INTERVAL '5 minutes' THEN 'online'
           WHEN last_seen IS NOT NULL THEN 'offline'
           ELSE 'unknown'
       END AS status,
       last_seen,
       passphrase_rotated_at,
       registered_at,
       expertise
FROM agents;
