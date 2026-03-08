-- MEM-043 rollback
ALTER TABLE agents DROP COLUMN IF EXISTS tokens_used;
ALTER TABLE agents DROP COLUMN IF EXISTS token_budget;
ALTER TABLE agents DROP COLUMN IF EXISTS tokens_reset_at;

DELETE FROM config WHERE key IN ('virtual_agent_default_token_budget', 'virtual_agent_budget_reset_days');

-- Restore previous agent_status view (from MEM-039)
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
