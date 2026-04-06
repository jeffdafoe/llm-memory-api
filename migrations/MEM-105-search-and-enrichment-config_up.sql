-- MEM-105: Add config keys for search tuning and enrichment limits.
-- These were previously hardcoded; now tunable from the admin UI.

INSERT INTO config (key, value, description) VALUES
    ('search_pool_multiplier', '3', 'Candidate pool multiplier for search. Fetches N times more results from pgvector, applies all boosts, then trims to requested limit. Higher = better recall, slightly slower.'),
    ('search_filename_boost', '0.15', 'Score boost when a search query word matches the source filename.'),
    ('search_bm25_boost', '0.1', 'Scale factor for BM25 full-text search boost. Only applies when vector similarity > 0.3.'),
    ('enrichment_max_relations', '15', 'Maximum number of relations the enrichment LLM can suggest per note.')
ON CONFLICT (key) DO NOTHING;
