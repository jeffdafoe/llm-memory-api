-- Drop the provider check constraint entirely.
-- Provider validation is handled by the provider registry in code.
ALTER TABLE agent_configuration DROP CONSTRAINT IF EXISTS chk_agent_configuration_provider;
