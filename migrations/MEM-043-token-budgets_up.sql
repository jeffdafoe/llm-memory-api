-- MEM-043: Token budgets for virtual agents
-- Tracks token usage per agent with configurable budgets and auto-reset.

ALTER TABLE agents ADD COLUMN tokens_used INTEGER DEFAULT 0;
ALTER TABLE agents ADD COLUMN token_budget INTEGER;
ALTER TABLE agents ADD COLUMN tokens_reset_at TIMESTAMP DEFAULT NOW();

INSERT INTO config (key, value) VALUES ('virtual_agent_default_token_budget', '1000000');
INSERT INTO config (key, value) VALUES ('virtual_agent_budget_reset_days', '30');

-- Update agent_status view to include token columns
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
       END AS active_since,
       tokens_used,
       token_budget,
       tokens_reset_at
FROM agents;
