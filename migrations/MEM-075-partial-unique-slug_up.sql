-- MEM-075: Replace unconditional unique constraint on (namespace, slug) with a
-- partial unique index that only enforces uniqueness on non-deleted rows.
-- This allows soft-deleted notes to coexist with active notes at the same slug,
-- which is required for the overwrite-on-rename feature.

-- Drop the old unconditional unique constraint
ALTER TABLE documents DROP CONSTRAINT IF EXISTS documents_namespace_slug_key;

-- Create a partial unique index that only covers active (non-deleted) rows
CREATE UNIQUE INDEX documents_namespace_slug_active_key
    ON documents (namespace, slug)
    WHERE deleted_at IS NULL;
