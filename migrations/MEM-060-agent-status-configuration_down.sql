-- MEM-060 down: Remove configuration column from agent_status view

CREATE OR REPLACE VIEW agent_status AS
SELECT
    ac.id AS actor_id,
    ac.name AS agent,
    CASE
        WHEN agc.virtual = true THEN 'available'
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
