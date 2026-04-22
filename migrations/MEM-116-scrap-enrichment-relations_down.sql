-- Reverse MEM-116: recreates the note_relations table and the config keys.
-- Data is not recoverable — the down migration exists for structural rollback only.

CREATE TABLE IF NOT EXISTS note_relations (
    id SERIAL PRIMARY KEY,
    source_namespace VARCHAR(64) NOT NULL,
    source_slug VARCHAR(255) NOT NULL,
    target_namespace VARCHAR(64) NOT NULL,
    target_slug VARCHAR(255) NOT NULL,
    relation_type VARCHAR(50) NOT NULL,
    auto_extracted BOOLEAN NOT NULL DEFAULT FALSE,
    created_by_actor_id INTEGER REFERENCES actors(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    metadata JSONB,
    UNIQUE(source_namespace, source_slug, target_namespace, target_slug, relation_type)
);

CREATE INDEX IF NOT EXISTS idx_note_relations_source ON note_relations(source_namespace, source_slug);
CREATE INDEX IF NOT EXISTS idx_note_relations_target ON note_relations(target_namespace, target_slug);
CREATE INDEX IF NOT EXISTS idx_note_relations_type ON note_relations(relation_type);

INSERT INTO config (key, value, description) VALUES
    ('enrichment_max_relations', '15', 'Maximum number of relations the enrichment LLM can suggest per note.'),
    ('enrichment_neighbor_count', '25', 'Number of similar notes fetched as relation-target candidates for enrichment.'),
    ('search_graph_boost', '0.05', 'Score boost for search results connected to top results via note relations. 0 = disabled.'),
    ('search_graph_decay', '0.5', 'Per-hop decay factor for spreading activation. 0.5 = each hop halves the activation. Range 0-1.'),
    ('search_graph_hops', '2', 'Maximum hop depth for spreading activation (1-4).')
ON CONFLICT (key) DO NOTHING;
