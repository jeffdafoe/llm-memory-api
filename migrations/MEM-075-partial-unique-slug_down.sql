-- MEM-075 rollback: restore unconditional unique constraint
DROP INDEX IF EXISTS documents_namespace_slug_active_key;
ALTER TABLE documents ADD CONSTRAINT documents_namespace_slug_key UNIQUE (namespace, slug);
