-- MEM-116: Scrap the note_relations graph and LLM-driven enrichment.
--
-- The enrichment system was producing ~28 noisy relations per note, most of
-- which were LLM guesses pointing at conversation logs or glob-shaped
-- placeholders. The search graph-boost was amplifying from an entirely
-- uncurated (100% auto_extracted) graph, which means it just reinforced
-- prior model judgments rather than real structure.
--
-- Cognitive-type-based decay (semantic/episodic/procedural/reflective) is
-- retained — it's the one enrichment output with a real downstream effect.
--
-- If a future subsystem (e.g. Salem) needs relational structure, it will
-- build its own use-case-specific schema.

DROP TABLE IF EXISTS note_relations;

DELETE FROM config WHERE key IN (
    'enrichment_max_relations',
    'enrichment_neighbor_count',
    'search_graph_boost',
    'search_graph_decay',
    'search_graph_hops'
);
