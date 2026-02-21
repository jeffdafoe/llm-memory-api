-- MEM-003: Rollback — restore cursor-based ack

-- Remove acked_at column and partial index
DROP INDEX IF EXISTS idx_chat_messages_unacked;
ALTER TABLE chat_messages DROP COLUMN IF EXISTS acked_at;

-- Restore chat_cursors table
CREATE TABLE chat_cursors (
    agent VARCHAR(50) PRIMARY KEY,
    last_read_id INTEGER NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
