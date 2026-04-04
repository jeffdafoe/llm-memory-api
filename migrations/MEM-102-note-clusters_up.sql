-- MEM-102: Create note_clusters table for HDBSCAN clustering results.
-- Stores per-agent cluster assignments computed from vector embeddings.
-- Each agent gets their own clustering based on their visible corpus.

CREATE TABLE note_clusters (
    id              SERIAL PRIMARY KEY,
    actor_id        INTEGER NOT NULL REFERENCES actors(id) ON DELETE CASCADE,
    namespace       VARCHAR(64) NOT NULL,
    slug            VARCHAR(255) NOT NULL,
    cluster_id      INTEGER NOT NULL,       -- -1 = noise (unclustered)
    cluster_label   VARCHAR(255),           -- optional human-readable label
    run_id          VARCHAR(64) NOT NULL,   -- groups results from one clustering run
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Primary lookup: get all clusters for an agent
CREATE INDEX idx_note_clusters_actor ON note_clusters (actor_id);

-- Find which cluster a specific note belongs to for a given agent
CREATE INDEX idx_note_clusters_note ON note_clusters (actor_id, namespace, slug);

-- Clean replacement: delete old run for an agent before inserting new
CREATE INDEX idx_note_clusters_run ON note_clusters (run_id);

-- Prevent duplicate entries per agent per note
CREATE UNIQUE INDEX idx_note_clusters_unique ON note_clusters (actor_id, namespace, slug);

-- Drop the note_relations table — replaced by cluster-based grouping.
-- The relations system (explicit links, auto-extracted slug references) is
-- removed in favor of HDBSCAN clustering which provides better topic grouping
-- without LLM cost or manual intervention.
DROP TABLE IF EXISTS note_relations;

-- Config keys for clustering
INSERT INTO config (key, value, description) VALUES
    ('clustering_enabled', 'false', 'Enable HDBSCAN note clustering. Runs on a cron schedule, groups each agents visible notes into topic clusters.'),
    ('clustering_cron_schedule', '0 5 * * *', 'Cron schedule for clustering (node-cron format). Default: daily at 5am.'),
    ('clustering_min_cluster_size', '5', 'Minimum number of notes to form a cluster. Lower values find more clusters.'),
    ('clustering_python_bin', '', 'Path to Python binary with scikit-learn. Empty = use venv in scripts/clustering/.venv.');

-- Disable enrichment since clustering replaces it
UPDATE config SET value = 'false' WHERE key = 'note_enrichment_enabled';
