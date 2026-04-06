-- MEM-101: Convert actors.expertise from text to jsonb
-- Prevents invalid JSON from being stored. The column was text with a
-- default of '[]' and all API routes already JSON.stringify before writing,
-- but a direct insert or UI edge case produced invalid JSON ([memory-enrichment]
-- without quotes) which broke the dream cron's jsonb cast.
--
-- The agent_status view depends on the expertise column, so it must be
-- dropped before ALTER TYPE and recreated after.

-- Drop the dependent view
DROP VIEW IF EXISTS agent_status;

-- Convert the column type. Existing valid JSON text values cast cleanly.
ALTER TABLE actors ALTER COLUMN expertise TYPE jsonb USING expertise::jsonb;

-- Update the default to be a jsonb literal instead of a text literal.
ALTER TABLE actors ALTER COLUMN expertise SET DEFAULT '[]'::jsonb;

-- Recreate the view (matches MEM-095 definition)
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
