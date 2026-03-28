-- Re-add the provider check constraint with all known providers.
ALTER TABLE agent_configuration
    ADD CONSTRAINT chk_agent_configuration_provider
        CHECK (provider IS NULL OR provider IN ('anthropic', 'google', 'openai', 'perplexity', 'xai'));
