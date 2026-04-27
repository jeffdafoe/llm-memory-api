-- MEM-120: Convert dream_mode to a proper enum type and add 'sim'
--
-- Sim NPCs (Salem village agents) need a third dream pipeline distinct from
-- companion/technical so the dream-cron processes their activity through
-- sim-flavored writers (dream-sim, dream-sim-soul, dream-sim-people) rather
-- than the companion-mode pipeline which biases souls toward "the user comes
-- often, quiet as a shadow"-style framing.
--
-- Since we're touching this anyway, convert dream_mode from VARCHAR(20) +
-- CHECK constraint to a proper Postgres ENUM type. Self-documenting at the
-- schema level. Adding values later is a one-line ALTER TYPE ... ADD VALUE.
-- The agent_status view depends on the column, so it must be dropped before
-- ALTER COLUMN TYPE and recreated after.

-- Drop the old CHECK constraint
ALTER TABLE agent_configuration DROP CONSTRAINT IF EXISTS chk_agent_configuration_dream_mode;

-- Drop the dependent view
DROP VIEW IF EXISTS agent_status;

-- Drop the column default so the type change isn't blocked by it
ALTER TABLE agent_configuration ALTER COLUMN dream_mode DROP DEFAULT;

-- Create the enum type with all four values
CREATE TYPE dream_mode_t AS ENUM ('none', 'companion', 'technical', 'sim');

-- Convert the column. Existing string values cast cleanly to matching enum labels.
ALTER TABLE agent_configuration ALTER COLUMN dream_mode TYPE dream_mode_t
    USING dream_mode::dream_mode_t;

-- Restore default (now an enum literal)
ALTER TABLE agent_configuration ALTER COLUMN dream_mode SET DEFAULT 'none'::dream_mode_t;

-- Recreate the view (matches MEM-101 definition)
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
