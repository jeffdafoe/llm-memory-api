-- MEM-094: Configurable conversation chunking parameters.
-- Smaller windows produce more focused embeddings for better search relevance.

INSERT INTO config (key, value, description) VALUES
    ('conversation_chunk_window', '5', 'Max messages per conversation chunk for vector search (default: 5). Smaller = more focused embeddings.'),
    ('conversation_chunk_overlap', '2', 'Messages carried over between conversation chunks for context continuity (default: 2).'),
    ('conversation_chunk_max_chars', '2000', 'Max characters per conversation chunk (default: 2000). Window closes early if exceeded.')
ON CONFLICT (key) DO NOTHING;
