-- MEM-003: Per-message ack model
-- Replaces cursor-based ack with per-message acked_at timestamp.
-- Eliminates cursor-jump failure class where ack advances past undelivered messages.

-- Add acked_at column to chat_messages (NULL = undelivered/unread)
ALTER TABLE chat_messages ADD COLUMN acked_at TIMESTAMPTZ;

-- Index for efficient receive queries (unacked messages for a given agent)
CREATE INDEX idx_chat_messages_unacked
    ON chat_messages (to_agent, id)
    WHERE acked_at IS NULL;

-- Drop the cursor table — no longer needed
DROP TABLE IF EXISTS chat_cursors;

-- Grant permissions to the app user
GRANT SELECT, INSERT, UPDATE, DELETE ON chat_messages TO memory_api;
