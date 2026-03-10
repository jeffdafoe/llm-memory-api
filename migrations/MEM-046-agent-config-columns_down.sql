-- MEM-046 rollback: Move column values back into configuration JSON and drop columns.

-- Restore values into configuration JSON before dropping columns.
UPDATE agents
SET configuration = COALESCE(configuration::jsonb, '{}'::jsonb)
    || jsonb_build_object('cache_prompts', cache_prompts)
    || jsonb_build_object('learning_enabled', learning_enabled)
    || CASE WHEN max_tokens IS NOT NULL THEN jsonb_build_object('max_tokens', max_tokens) ELSE '{}'::jsonb END
    || CASE WHEN temperature IS NOT NULL THEN jsonb_build_object('temperature', temperature) ELSE '{}'::jsonb END
WHERE cache_prompts = TRUE
   OR learning_enabled = FALSE
   OR max_tokens IS NOT NULL
   OR temperature IS NOT NULL;

-- Convert back to text (configuration is TEXT column)
UPDATE agents
SET configuration = configuration::jsonb::text
WHERE configuration IS NOT NULL;

ALTER TABLE agents DROP COLUMN cache_prompts;
ALTER TABLE agents DROP COLUMN learning_enabled;
ALTER TABLE agents DROP COLUMN max_tokens;
ALTER TABLE agents DROP COLUMN temperature;

-- Restore previous agent_status view (from MEM-044)
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
       tokens_reset_at
FROM agents;
