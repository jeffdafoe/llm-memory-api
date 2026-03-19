-- MEM-064: Conversation sync support.
-- Adds metadata jsonb column to documents, seeds conversation-related config.

-- 1. Add metadata column (general-purpose structured metadata for any note)
ALTER TABLE documents ADD COLUMN metadata JSONB;

-- 2. Backfill kind for any existing conversation notes (unlikely, but safe)
UPDATE documents SET kind = 'conversation' WHERE slug LIKE 'conversations/%' AND kind != 'conversation';

-- 3. Seed conversation config
INSERT INTO config (key, value, description) VALUES
    ('search_decay_halflife_conversation', '30', 'Half-life in days for conversation note search decay'),
    ('search_conversation_weight', '0.7', 'Score multiplier for conversation search results (0.0-1.0). Prevents conversations from outranking curated notes.'),
    ('conversation_retention_days', '30', 'Number of days of conversation history to retain during sync')
ON CONFLICT (key) DO NOTHING;
