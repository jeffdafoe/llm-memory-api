-- MEM-063: Search decay & access boost for vector search.
-- Adds kind and last_accessed columns to documents, seeds config defaults.

-- 1. Add columns
ALTER TABLE documents ADD COLUMN kind VARCHAR(20);
ALTER TABLE documents ADD COLUMN last_accessed TIMESTAMPTZ;

-- 2. Backfill kind from slug prefix
UPDATE documents SET kind = CASE
    WHEN slug LIKE 'instructions/%' THEN 'instruction'
    WHEN slug LIKE 'notes/codebase/%' THEN 'reference'
    WHEN slug LIKE 'tasks/done/%' THEN 'task'
    WHEN slug LIKE 'tasks/%' THEN 'task'
    WHEN slug LIKE 'learnings/%' THEN 'learning'
    WHEN slug LIKE 'notes/%' THEN 'note'
    ELSE 'note'
END;

-- 3. Set default and NOT NULL (all rows backfilled above, code always sets kind)
ALTER TABLE documents ALTER COLUMN kind SET DEFAULT 'note';
ALTER TABLE documents ALTER COLUMN kind SET NOT NULL;

-- 4. Index for the LEFT JOIN in searchMemory (namespace + case-insensitive slug)
CREATE INDEX IF NOT EXISTS documents_namespace_lower_slug_idx ON documents (namespace, LOWER(slug));

-- 5. Seed search config
INSERT INTO config (key, value, description) VALUES
    ('search_decay_halflife_task', '60', 'Half-life in days for task note search decay'),
    ('search_decay_halflife_learning', '90', 'Half-life in days for learning note search decay'),
    ('search_decay_halflife_note', '180', 'Half-life in days for general note search decay'),
    ('search_decay_halflife_reference', '0', 'Half-life in days for reference note search decay (0 = no decay)'),
    ('search_decay_halflife_instruction', '0', 'Half-life in days for instruction note search decay (0 = no decay)'),
    ('search_access_boost_max', '0.05', 'Maximum additive boost for recently accessed notes'),
    ('search_access_boost_window_days', '14', 'Days within which access boost applies')
ON CONFLICT (key) DO NOTHING;
