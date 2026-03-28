-- Remove xAI from the allowed provider values (revert to previous list).
-- Only safe if no rows use 'xai' as provider.
ALTER TABLE agent_configuration
    DROP CONSTRAINT chk_agent_configuration_provider,
    ADD CONSTRAINT chk_agent_configuration_provider
        CHECK (provider IS NULL OR provider IN ('anthropic', 'google', 'openai', 'perplexity'));
