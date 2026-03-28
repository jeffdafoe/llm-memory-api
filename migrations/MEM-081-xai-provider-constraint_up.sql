-- Add xAI to the allowed provider values in agent_configuration.
ALTER TABLE agent_configuration
    DROP CONSTRAINT chk_agent_configuration_provider,
    ADD CONSTRAINT chk_agent_configuration_provider
        CHECK (provider IS NULL OR provider IN ('anthropic', 'google', 'openai', 'perplexity', 'xai'));
