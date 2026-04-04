-- MEM-103: Lightweight slug reference tracking.
-- Stores auto-extracted slug mentions found in note content.
-- No relation types, no manual creation — purely mechanical extraction.

CREATE TABLE slug_references (
    id              SERIAL PRIMARY KEY,
    source_namespace VARCHAR(64) NOT NULL,
    source_slug     VARCHAR(255) NOT NULL,
    target_namespace VARCHAR(64) NOT NULL,
    target_slug     VARCHAR(255) NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Lookup: find what a note references
CREATE INDEX idx_slug_refs_source ON slug_references (source_namespace, source_slug);

-- Lookup: find what references a note
CREATE INDEX idx_slug_refs_target ON slug_references (target_namespace, target_slug);

-- Prevent duplicates
CREATE UNIQUE INDEX idx_slug_refs_unique ON slug_references
    (source_namespace, source_slug, target_namespace, target_slug);
