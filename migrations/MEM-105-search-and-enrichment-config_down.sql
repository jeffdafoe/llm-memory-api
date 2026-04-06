-- MEM-105 down: Remove search tuning and enrichment config keys.

DELETE FROM config WHERE key IN (
    'search_pool_multiplier',
    'search_filename_boost',
    'search_bm25_boost',
    'enrichment_max_relations'
);
