-- MEM-038: CHECK constraint on agents.provider column.
-- Restricts to supported provider values.

ALTER TABLE agents ADD CONSTRAINT chk_agents_provider
    CHECK (provider IS NULL OR provider IN ('anthropic', 'google', 'openai'));
