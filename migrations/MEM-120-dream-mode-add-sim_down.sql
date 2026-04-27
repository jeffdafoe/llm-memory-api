-- MEM-120 down: Revert dream_mode from enum back to VARCHAR(20) + CHECK constraint
-- with the original ('none', 'companion', 'technical') value set.
--
-- Reset any sim-mode agents to 'none' first so the down migration doesn't
-- leave rows that violate the restored CHECK constraint.

-- Reset sim rows so they fit the restored constraint
UPDATE agent_configuration SET dream_mode = 'none' WHERE dream_mode = 'sim';

-- Drop the dependent view (column type change requires it)
DROP VIEW IF EXISTS agent_status;

-- Drop the enum default so the type change isn't blocked
ALTER TABLE agent_configuration ALTER COLUMN dream_mode DROP DEFAULT;

-- Convert the column back to VARCHAR(20)
ALTER TABLE agent_configuration ALTER COLUMN dream_mode TYPE VARCHAR(20)
    USING dream_mode::text;

-- Restore the text default
ALTER TABLE agent_configuration ALTER COLUMN dream_mode SET DEFAULT 'none';

-- Re-add the original CHECK constraint (with only the three original values)
ALTER TABLE agent_configuration ADD CONSTRAINT chk_agent_configuration_dream_mode
    CHECK (dream_mode IN ('none', 'companion', 'technical'));

-- Drop the now-unused enum type
DROP TYPE IF EXISTS dream_mode_t;

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
