-- LLM-436: Relevance floor for memory search.
-- Search was a pure top-k ranker — against a small corpus (e.g. an NPC's
-- private memory partition) it returned the nearest neighbor no matter how
-- unrelated. This key floors the composite similarity score; hits below it
-- are dropped. Measured on live data: pure noise scores <= ~0.25, legitimate
-- fuzzy recalls >= ~0.37 — 0.3 is the recommended operating value.

INSERT INTO config (key, value, description) VALUES
    ('search_min_similarity', '0', 'Minimum composite similarity for search results; hits scoring below are dropped instead of padding the top-k. 0 = off. Measured: noise <= ~0.25, legit fuzzy recalls >= ~0.37; 0.3 recommended.')
ON CONFLICT (key) DO NOTHING;
