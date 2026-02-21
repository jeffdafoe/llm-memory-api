-- MEM-002: Rollback — restore namespace naming in mail, drop agents, restore channel-based chat

DROP TABLE IF EXISTS chat_cursors;
DROP TABLE IF EXISTS chat_messages;
DROP TABLE IF EXISTS agents;

-- Restore mail column names
ALTER TABLE mail RENAME COLUMN from_agent TO from_namespace;
ALTER TABLE mail RENAME COLUMN to_agent TO to_namespace;
ALTER INDEX idx_mail_to_agent_acked RENAME TO idx_mail_to_namespace_acked;

-- Restore channel-based chat
CREATE TABLE chat_messages (
    id SERIAL PRIMARY KEY,
    channel VARCHAR(100) NOT NULL,
    from_namespace VARCHAR(50) NOT NULL,
    message TEXT NOT NULL,
    sent_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_chat_messages_channel
    ON chat_messages (channel, id);
