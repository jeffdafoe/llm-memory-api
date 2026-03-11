-- MEM-051: Clear seeded default config from agents.
-- The admin UI previously copied provider defaults into the configuration column
-- and promoted columns (temperature, max_tokens) at creation time. These should
-- be NULL so agents inherit from provider defaults, which can be updated centrally.

UPDATE agents
SET configuration = NULL,
    temperature = NULL,
    max_tokens = NULL
WHERE agent = 'search-general';
