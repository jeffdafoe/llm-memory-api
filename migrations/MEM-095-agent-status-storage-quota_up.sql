-- MEM-095: Add storage_quota to agent_status view so the admin dashboard
-- can display per-agent quota alongside the global default.

CREATE OR REPLACE VIEW agent_status AS
SELECT ac.id AS actor_id,
    ac.name AS agent,
    CASE
        WHEN agc.virtual = TRUE AND ac.status IN ('available', 'degraded', 'error') THEN ac.status
        WHEN agc.virtual = TRUE THEN 'available'
        WHEN ac.last_seen > (now() - INTERVAL '15 minutes') THEN 'online'
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
        WHEN ac.active_since IS NOT NULL AND ac.active_since > (now() - INTERVAL '30 minutes') THEN ac.active_since
        ELSE NULL
    END AS active_since,
    agc.cache_prompts,
    agc.learning_enabled,
    agc.max_tokens,
    agc.temperature,
    agc.dream_mode,
    agc.storage_quota
FROM actors ac
JOIN agent_configuration agc ON agc.actor_id = ac.id;
