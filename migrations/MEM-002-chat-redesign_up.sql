-- MEM-002: Agent registration + chat redesign
-- Adds agents table, renames namespace→agent in mail, replaces channel-based chat with 1:1 messaging + ack-based read tracking

CREATE TABLE agents (
    agent VARCHAR(50) PRIMARY KEY,
    registered_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Rename mail columns from namespace to agent
ALTER TABLE mail RENAME COLUMN from_namespace TO from_agent;
ALTER TABLE mail RENAME COLUMN to_namespace TO to_agent;
ALTER INDEX idx_mail_to_namespace_acked RENAME TO idx_mail_to_agent_acked;

-- Drop old channel-based chat
DROP INDEX IF EXISTS idx_chat_messages_channel;
DROP TABLE IF EXISTS chat_messages;

-- New 1:1 chat
CREATE TABLE chat_messages (
    id SERIAL PRIMARY KEY,
    from_agent VARCHAR(50) NOT NULL,
    to_agent VARCHAR(50) NOT NULL,
    message TEXT NOT NULL,
    sent_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_chat_messages_to_agent
    ON chat_messages (to_agent, id);

CREATE TABLE chat_cursors (
    agent VARCHAR(50) PRIMARY KEY,
    last_read_id INTEGER NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
