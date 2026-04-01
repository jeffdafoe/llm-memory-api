-- MEM-088: Add dream_mode to agent_configuration
-- Controls post-session conversation log analysis behavior per agent.
-- Values: 'none' (disabled), 'companion' (emotional/personal), 'technical' (work/code-focused)

ALTER TABLE agent_configuration ADD COLUMN dream_mode VARCHAR(20) NOT NULL DEFAULT 'none';

ALTER TABLE agent_configuration ADD CONSTRAINT chk_agent_configuration_dream_mode
    CHECK (dream_mode IN ('none', 'companion', 'technical'));

-- Tracks when this agent was last processed by the dream job
ALTER TABLE agent_configuration ADD COLUMN last_dream_at TIMESTAMPTZ;

-- Global switch for dream processing
INSERT INTO config (key, value, description) VALUES ('dream_processing_enabled', 'false', 'Global switch for dream processing. Set to true to enable nightly conversation log analysis.');

INSERT INTO config (key, value, description) VALUES ('dream_cron_schedule', '', 'Cron expression for dream processing schedule (e.g. 0 3 * * * for 3am daily). Empty to disable.');

INSERT INTO config (key, value, description) VALUES ('dream_bootstrap', 'The dream system is enabled for your account. Each night, your conversation logs are analyzed and consolidated into memory notes under dreams/ in your namespace.', 'Text appended to agent instructions when dream mode is enabled. Empty to skip.');

-- Recreate agent_status view to include dream_mode
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
    agc.dream_mode
FROM actors ac
JOIN agent_configuration agc ON agc.actor_id = ac.id;

-- Search tuning for dream notes
INSERT INTO config (key, value, description) VALUES ('search_decay_halflife_dream', '30', 'Half-life in days for dream note search decay. Notes lose 50% relevance after this many days. 0 to disable decay.');
INSERT INTO config (key, value, description) VALUES ('search_dream_weight', '1.0', 'Search weight multiplier for dream notes. 1.0 = no penalty, lower values reduce ranking relative to other note types.');
