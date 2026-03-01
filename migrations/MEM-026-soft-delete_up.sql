-- MEM-026: Soft delete for documents
-- Adds deleted_at column. The existing unique constraint on (namespace, slug) stays —
-- saveNote's upsert will clear deleted_at when overwriting a soft-deleted row.

ALTER TABLE documents ADD COLUMN deleted_at TIMESTAMPTZ;
