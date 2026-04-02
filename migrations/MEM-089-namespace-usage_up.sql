-- Namespace storage usage tracking.
-- Tracks note count and total content bytes per namespace.
-- Updated incrementally by document mutation hooks in documents.js.

CREATE TABLE namespace_usage (
    namespace VARCHAR(64) PRIMARY KEY,
    note_count INTEGER NOT NULL DEFAULT 0,
    total_bytes BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ DEFAULT NOW()
);

-- Seed from existing data (only active/non-deleted notes)
INSERT INTO namespace_usage (namespace, note_count, total_bytes, updated_at)
SELECT namespace,
       COUNT(*),
       COALESCE(SUM(LENGTH(content)), 0),
       NOW()
FROM documents
WHERE deleted_at IS NULL
GROUP BY namespace;
