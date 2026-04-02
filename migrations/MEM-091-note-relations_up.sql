-- Note graph relations — structural connections between notes.
-- Supports manual relations (created by agents) and auto-extracted
-- references (slug mentions detected on save).

CREATE TABLE note_relations (
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

CREATE INDEX idx_note_relations_source ON note_relations(source_namespace, source_slug);
CREATE INDEX idx_note_relations_target ON note_relations(target_namespace, target_slug);
CREATE INDEX idx_note_relations_type ON note_relations(relation_type);

-- Search graph boost: score bump for results connected to top results via relations.
-- 0 = disabled, 0.05-0.1 = gentle boost. Applied as post-processing after vector search.
INSERT INTO config (key, value, description) VALUES
    ('search_graph_boost', '0.05', 'Score boost for search results connected to top results via note relations. 0 = disabled.')
ON CONFLICT (key) DO NOTHING;
