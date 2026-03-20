-- Add status column to actors for virtual agent lifecycle tracking
ALTER TABLE actors ADD COLUMN status VARCHAR(20) DEFAULT NULL;

-- Add config keys for retry and error ping
INSERT INTO config (key, value, description) VALUES
    ('virtual_agent_max_retries', '3', 'Number of retry attempts for virtual agent provider calls before giving up'),
    ('virtual_agent_retry_backoff', '300,600,3600', 'Comma-separated backoff delays in seconds between retries'),
    ('virtual_agent_error_ping_interval', '15', 'Minutes between health-check pings for errored virtual agents');

-- Recreate agent_status view to use actors.status for virtual agents
DROP VIEW IF EXISTS agent_status;
CREATE VIEW agent_status AS
SELECT ac.id AS actor_id,
       ac.name AS agent,
       CASE
           WHEN agc.virtual = TRUE AND ac.status IS NOT NULL THEN ac.status
           WHEN agc.virtual = TRUE THEN 'available'
           WHEN ac.last_seen > NOW() - INTERVAL '15 minutes' THEN 'online'
           WHEN ac.last_seen IS NOT NULL THEN 'offline'
           ELSE 'unknown'
       END AS status,
       ac.last_seen,
       ac.passphrase_rotated_at,
       ac.created_at AS registered_at,
       ac.expertise,
       agc.provider,
       agc.model,
       agc.virtual,
       agc.personality,
       agc.cost_budget_daily,
       agc.cost_budget_monthly,
       CASE
           WHEN ac.active_since IS NOT NULL AND ac.active_since > NOW() - INTERVAL '30 minutes' THEN ac.active_since
           ELSE NULL
       END AS active_since,
       agc.cache_prompts,
       agc.learning_enabled,
       agc.max_tokens,
       agc.temperature
FROM actors ac
JOIN agent_configuration agc ON agc.actor_id = ac.id;
