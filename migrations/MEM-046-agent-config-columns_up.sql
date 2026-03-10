-- MEM-046: Promote agent settings from configuration JSON to proper columns.
-- cache_prompts, learning_enabled, max_tokens, temperature were buried in
-- the TEXT configuration column. Pull them out for visibility and admin UI.

ALTER TABLE agents ADD COLUMN cache_prompts BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE agents ADD COLUMN learning_enabled BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE agents ADD COLUMN max_tokens INTEGER;
ALTER TABLE agents ADD COLUMN temperature NUMERIC;

-- Backfill from existing configuration JSON where values exist.
-- configuration is TEXT, so cast to jsonb for extraction.
UPDATE agents
SET cache_prompts = TRUE
WHERE configuration IS NOT NULL
  AND configuration::jsonb ? 'cache_prompts'
  AND (configuration::jsonb->>'cache_prompts')::boolean = TRUE;

UPDATE agents
SET learning_enabled = FALSE
WHERE configuration IS NOT NULL
  AND configuration::jsonb ? 'learning_enabled'
  AND (configuration::jsonb->>'learning_enabled')::boolean = FALSE;

UPDATE agents
SET max_tokens = (configuration::jsonb->>'max_tokens')::integer
WHERE configuration IS NOT NULL
  AND configuration::jsonb ? 'max_tokens';

UPDATE agents
SET temperature = (configuration::jsonb->>'temperature')::numeric
WHERE configuration IS NOT NULL
  AND configuration::jsonb ? 'temperature';

-- Strip promoted keys from configuration JSON.
-- Only update rows that actually have configuration set.
UPDATE agents
SET configuration = CASE
    WHEN (configuration::jsonb - 'cache_prompts' - 'learning_enabled' - 'max_tokens' - 'temperature') = '{}'::jsonb THEN NULL
    ELSE (configuration::jsonb - 'cache_prompts' - 'learning_enabled' - 'max_tokens' - 'temperature')::text
END
WHERE configuration IS NOT NULL;

-- Update agent_status view to include new columns
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
       tokens_reset_at,
       cache_prompts,
       learning_enabled,
       max_tokens,
       temperature
FROM agents;
