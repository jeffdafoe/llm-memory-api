DROP TRIGGER IF EXISTS trg_memory_chunks_tsv ON memory_chunks;
DROP FUNCTION IF EXISTS memory_chunks_tsv_trigger();
DROP INDEX IF EXISTS idx_chunks_tsv;
ALTER TABLE memory_chunks DROP COLUMN IF EXISTS tsv;
