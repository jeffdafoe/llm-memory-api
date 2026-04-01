-- MEM-088 down: Remove dream_mode from agent_configuration

ALTER TABLE agent_configuration DROP CONSTRAINT IF EXISTS chk_agent_configuration_dream_mode;
ALTER TABLE agent_configuration DROP COLUMN IF EXISTS dream_mode;
ALTER TABLE agent_configuration DROP COLUMN IF EXISTS last_dream_at;
DELETE FROM config WHERE key IN ('dream_processing_enabled', 'search_decay_halflife_dream', 'search_dream_weight');
