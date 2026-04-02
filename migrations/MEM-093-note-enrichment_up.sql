-- MEM-093: Add note enrichment config key.
-- When enabled and a "memory-enrichment" virtual agent exists, saved notes
-- are analyzed by the agent to generate keywords, tags, and suggested relations.

INSERT INTO config (key, value, description) VALUES
    ('note_enrichment_enabled', 'false', 'Enable LLM-powered note enrichment on save. Requires a virtual agent named "memory-enrichment". Keywords/tags stored in note metadata, relations created via note_relations.');
