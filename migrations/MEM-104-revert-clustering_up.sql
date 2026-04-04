-- MEM-104: Revert HDBSCAN clustering experiment.
-- Drop clustering and slug_references tables, restore note_relations,
-- remove clustering config keys.

-- Drop tables added by MEM-102 and MEM-103
DROP TABLE IF EXISTS note_clusters;
DROP TABLE IF EXISTS slug_references;

-- Remove clustering config keys
DELETE FROM config WHERE key IN (
    'clustering_enabled',
    'clustering_cron_schedule',
    'clustering_min_cluster_size',
    'clustering_python_bin'
);

-- Restore note_relations table (originally from MEM-091, dropped by MEM-102)
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

-- Restore search graph boost config if missing
INSERT INTO config (key, value, description) VALUES
    ('search_graph_boost', '0.05', 'Score boost for search results connected to top results via note relations. 0 = disabled.')
ON CONFLICT (key) DO NOTHING;

-- Configurable neighbor count for enrichment LLM context
INSERT INTO config (key, value, description) VALUES
    ('enrichment_neighbor_count', '10', 'Number of similar notes to include as context when enrichment LLM suggests relations. Higher = more context but more tokens.')
ON CONFLICT (key) DO NOTHING;
