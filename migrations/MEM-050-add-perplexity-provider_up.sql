-- MEM-050: Add perplexity to the provider CHECK constraint.
-- The perplexity provider module was added but the DB constraint wasn't updated.

ALTER TABLE agents DROP CONSTRAINT chk_agents_provider;
ALTER TABLE agents ADD CONSTRAINT chk_agents_provider
    CHECK (provider IS NULL OR provider IN ('anthropic', 'google', 'openai', 'perplexity'));
