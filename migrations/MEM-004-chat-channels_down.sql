-- MEM-004 rollback: Remove channel support

DROP INDEX IF EXISTS idx_chat_messages_unacked;

ALTER TABLE chat_messages DROP COLUMN IF EXISTS channel;

CREATE INDEX idx_chat_messages_unacked
    ON chat_messages (to_agent, id)
    WHERE acked_at IS NULL;
