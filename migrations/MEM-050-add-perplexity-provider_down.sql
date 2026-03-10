-- MEM-050 rollback: Remove perplexity from provider CHECK constraint.
-- Will fail if any agents have provider = 'perplexity'.

ALTER TABLE agents DROP CONSTRAINT chk_agents_provider;
ALTER TABLE agents ADD CONSTRAINT chk_agents_provider
    CHECK (provider IS NULL OR provider IN ('anthropic', 'google', 'openai'));
