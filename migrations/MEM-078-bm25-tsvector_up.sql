-- Add tsvector column for BM25/full-text search on memory_chunks
ALTER TABLE memory_chunks ADD COLUMN tsv tsvector;

-- Populate from existing chunk_text
UPDATE memory_chunks SET tsv = to_tsvector('english', chunk_text);

-- GIN index for fast full-text search
CREATE INDEX idx_chunks_tsv ON memory_chunks USING GIN (tsv);

-- Auto-update tsvector on insert/update
CREATE OR REPLACE FUNCTION memory_chunks_tsv_trigger() RETURNS trigger AS $$
BEGIN
    NEW.tsv := to_tsvector('english', NEW.chunk_text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_memory_chunks_tsv
    BEFORE INSERT OR UPDATE OF chunk_text ON memory_chunks
    FOR EACH ROW
    EXECUTE FUNCTION memory_chunks_tsv_trigger();
