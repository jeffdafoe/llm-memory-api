-- MEM-052: Replace token-counting budget with cost-based budget system.
-- Logs every API call with calculated cost, uses rolling daily/monthly windows.

-- 1. Create usage log table
CREATE TABLE virtual_agent_usage (
    id SERIAL PRIMARY KEY,
    agent VARCHAR(50) NOT NULL REFERENCES agents(agent),
    provider VARCHAR(50),
    model VARCHAR(100),
    input_tokens INTEGER DEFAULT 0,
    output_tokens INTEGER DEFAULT 0,
    cache_write_tokens INTEGER DEFAULT 0,
    cache_read_tokens INTEGER DEFAULT 0,
    cost NUMERIC(10, 6) NOT NULL DEFAULT 0,
    context VARCHAR(50),
    created_at TIMESTAMPTZ DEFAULT NOW()
);
CREATE INDEX idx_va_usage_agent_date ON virtual_agent_usage (agent, created_at);

-- 2. Drop old token budget columns
ALTER TABLE agents DROP COLUMN tokens_used;
ALTER TABLE agents DROP COLUMN token_budget;
ALTER TABLE agents DROP COLUMN tokens_reset_at;

-- 3. Drop old cost text column
ALTER TABLE agents DROP COLUMN cost;

-- 4. Add new cost budget columns (NULL = use default from config)
ALTER TABLE agents ADD COLUMN cost_budget_daily NUMERIC(10, 2);
ALTER TABLE agents ADD COLUMN cost_budget_monthly NUMERIC(10, 2);

-- 5. Remove old config keys, add new ones
DELETE FROM config WHERE key IN ('virtual_agent_default_token_budget', 'virtual_agent_budget_reset_days');
INSERT INTO config (key, value, description) VALUES ('virtual_agent_default_daily_budget', '1.00', 'Default daily cost limit in dollars for virtual agents');
INSERT INTO config (key, value, description) VALUES ('virtual_agent_default_monthly_budget', '10.00', 'Default 30-day rolling cost limit in dollars for virtual agents');

-- 6. Recreate agent_status view without removed columns, with new budget columns
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
       cost_budget_daily,
       cost_budget_monthly,
       CASE
           WHEN active_since IS NOT NULL AND active_since > NOW() - INTERVAL '30 minutes' THEN active_since
           ELSE NULL
       END AS active_since,
       cache_prompts,
       learning_enabled,
       max_tokens,
       temperature
FROM agents;
