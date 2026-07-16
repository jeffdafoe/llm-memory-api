-- LLM-436 down: remove the search relevance-floor config key.

DELETE FROM config WHERE key = 'search_min_similarity';
