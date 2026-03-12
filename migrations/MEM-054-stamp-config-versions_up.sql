-- MEM-054: Stamp _configVersion into existing agent configurations.
-- Prevents stale-config errors after the configVersion check was added.
-- Anthropic Opus/Sonnet get v2 (thinking capability restructured), everything else gets v1.

-- Anthropic models that had capabilities restructured (extended_thinking -> thinking_effort)
UPDATE agents
SET configuration = jsonb_set(
    COALESCE(configuration::jsonb, '{}'::jsonb),
    '{_configVersion}',
    '2'
)::text
WHERE provider = 'anthropic'
  AND model IN ('claude-opus-4-6', 'claude-sonnet-4-6');

-- All other agents with a provider+model set get v1
UPDATE agents
SET configuration = jsonb_set(
    COALESCE(configuration::jsonb, '{}'::jsonb),
    '{_configVersion}',
    '1'
)::text
WHERE provider IS NOT NULL
  AND model IS NOT NULL
  AND NOT (provider = 'anthropic' AND model IN ('claude-opus-4-6', 'claude-sonnet-4-6'));
