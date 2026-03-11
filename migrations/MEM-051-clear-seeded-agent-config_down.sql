-- MEM-051 down: Restore search-general's original seeded config.
-- Values from perplexity sonar defaults at time of creation.

UPDATE agents
SET configuration = '{"search_recency_filter":"","return_citations":true}',
    temperature = 0.2,
    max_tokens = 1024
WHERE agent = 'search-general';
