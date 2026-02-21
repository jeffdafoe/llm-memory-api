-- MEM-004: Chat channel support
-- Adds optional channel column to chat_messages for stream isolation.
-- NULL channel = regular direct chat. Named channels (e.g. "discussion")
-- allow tools like the discussion transport to operate on a separate stream
-- without consuming regular chat messages.

ALTER TABLE chat_messages ADD COLUMN channel VARCHAR(50) DEFAULT NULL;

-- Replace the old unacked index with one that includes channel
DROP INDEX IF EXISTS idx_chat_messages_unacked;

CREATE INDEX idx_chat_messages_unacked
    ON chat_messages (to_agent, channel)
    WHERE acked_at IS NULL;
