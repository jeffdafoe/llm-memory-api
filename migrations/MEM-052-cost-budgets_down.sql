-- MEM-052 rollback: Restore token-counting budget system.

-- Drop usage log table
DROP TABLE IF EXISTS virtual_agent_usage;

-- Restore old columns
ALTER TABLE agents ADD COLUMN tokens_used INTEGER DEFAULT 0;
ALTER TABLE agents ADD COLUMN token_budget INTEGER;
ALTER TABLE agents ADD COLUMN tokens_reset_at TIMESTAMP DEFAULT NOW();
ALTER TABLE agents ADD COLUMN cost TEXT;

-- Drop new columns
ALTER TABLE agents DROP COLUMN IF EXISTS cost_budget_daily;
ALTER TABLE agents DROP COLUMN IF EXISTS cost_budget_monthly;

-- Restore old config keys
DELETE FROM config WHERE key IN ('virtual_agent_default_daily_budget', 'virtual_agent_default_monthly_budget');
INSERT INTO config (key, value) VALUES ('virtual_agent_default_token_budget', '1000000');
INSERT INTO config (key, value) VALUES ('virtual_agent_budget_reset_days', '30');

-- Recreate agent_status view with old columns
CREATE OR REPLACE VIEW agent_status AS
SELECT agent,
       CASE
           WHEN virtual = TRUE THEN 'available'
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
       END AS active_since,
       tokens_used,
       token_budget,
       tokens_reset_at,
       cache_prompts,
       learning_enabled,
       max_tokens,
       temperature
FROM agents;
