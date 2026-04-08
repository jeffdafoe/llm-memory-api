-- MEM-107: Split chat_messages into message texts + delivery rows
--
-- Before: chat_messages stores one row per recipient, duplicating message text.
-- After: chat_message_texts stores message content once (with sender, timestamp,
--        and optional discussion_id). chat_messages becomes a delivery table
--        referencing the text row.

BEGIN;

-- 1. Create the message text table
CREATE TABLE chat_message_texts (
    id SERIAL PRIMARY KEY,
    message TEXT NOT NULL,
    from_actor_id INTEGER NOT NULL REFERENCES actors(id),
    discussion_id INTEGER REFERENCES discussions(id),
    sent_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_cmt_discussion ON chat_message_texts (discussion_id) WHERE discussion_id IS NOT NULL;
CREATE INDEX idx_cmt_from_actor ON chat_message_texts (from_actor_id);
CREATE INDEX idx_cmt_sent_at ON chat_message_texts (sent_at);

GRANT ALL ON TABLE chat_message_texts TO memory_api;
GRANT SELECT, INSERT, DELETE, UPDATE ON TABLE chat_message_texts TO claude;
GRANT ALL ON SEQUENCE chat_message_texts_id_seq TO memory_api;
GRANT SELECT, USAGE ON SEQUENCE chat_message_texts_id_seq TO claude;

-- 2. Add message_text_id to chat_messages (nullable initially for migration)
ALTER TABLE chat_messages ADD COLUMN message_text_id INTEGER REFERENCES chat_message_texts(id);

-- 3. Migrate existing data:
--    For each unique (from_actor_id, message, channel, sent_at) combo,
--    insert one text row and link all delivery rows to it.
--    Extract discussion_id from channel string 'discussion-{N}'.
WITH grouped AS (
    SELECT
        from_actor_id,
        message,
        channel,
        sent_at,
        CASE
            WHEN channel LIKE 'discussion-%'
            THEN CAST(SUBSTRING(channel FROM 'discussion-(\d+)') AS INTEGER)
            ELSE NULL
        END AS discussion_id,
        ARRAY_AGG(id) AS delivery_ids
    FROM chat_messages
    WHERE deleted_at IS NULL
    GROUP BY from_actor_id, message, channel, sent_at
),
inserted AS (
    INSERT INTO chat_message_texts (message, from_actor_id, discussion_id, sent_at)
    SELECT message, from_actor_id, discussion_id, sent_at
    FROM grouped
    RETURNING id, from_actor_id, message, sent_at
)
UPDATE chat_messages cm
SET message_text_id = ins.id
FROM inserted ins
WHERE cm.from_actor_id = ins.from_actor_id
  AND cm.message = ins.message
  AND cm.sent_at = ins.sent_at
  AND cm.message_text_id IS NULL;

-- Also handle soft-deleted messages
WITH grouped AS (
    SELECT
        from_actor_id,
        message,
        channel,
        sent_at,
        CASE
            WHEN channel LIKE 'discussion-%'
            THEN CAST(SUBSTRING(channel FROM 'discussion-(\d+)') AS INTEGER)
            ELSE NULL
        END AS discussion_id,
        ARRAY_AGG(id) AS delivery_ids
    FROM chat_messages
    WHERE deleted_at IS NOT NULL AND message_text_id IS NULL
    GROUP BY from_actor_id, message, channel, sent_at
),
inserted AS (
    INSERT INTO chat_message_texts (message, from_actor_id, discussion_id, sent_at)
    SELECT message, from_actor_id, discussion_id, sent_at
    FROM grouped
    RETURNING id, from_actor_id, message, sent_at
)
UPDATE chat_messages cm
SET message_text_id = ins.id
FROM inserted ins
WHERE cm.from_actor_id = ins.from_actor_id
  AND cm.message = ins.message
  AND cm.sent_at = ins.sent_at
  AND cm.message_text_id IS NULL;

-- 4. Make message_text_id NOT NULL now that all rows are migrated
ALTER TABLE chat_messages ALTER COLUMN message_text_id SET NOT NULL;

-- 5. Drop the now-redundant columns from chat_messages
ALTER TABLE chat_messages DROP COLUMN message;
ALTER TABLE chat_messages DROP COLUMN from_actor_id;
ALTER TABLE chat_messages DROP COLUMN channel;
ALTER TABLE chat_messages DROP COLUMN sent_at;

-- 6. Add index on the FK
CREATE INDEX idx_cm_message_text ON chat_messages (message_text_id);

COMMIT;
