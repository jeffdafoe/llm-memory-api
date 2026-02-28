-- MEM-021: Add created_by to documents
-- Tracks which agent created the document. Matters when multiple agents share a namespace.

ALTER TABLE documents ADD COLUMN created_by VARCHAR(50) REFERENCES agents(agent);
