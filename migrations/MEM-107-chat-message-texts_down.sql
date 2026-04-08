-- MEM-107 down: Restore chat_messages to single-table design

BEGIN;

-- 1. Re-add the columns to chat_messages
ALTER TABLE chat_messages ADD COLUMN message TEXT;
ALTER TABLE chat_messages ADD COLUMN from_actor_id INTEGER REFERENCES actors(id);
ALTER TABLE chat_messages ADD COLUMN channel VARCHAR(50);
ALTER TABLE chat_messages ADD COLUMN sent_at TIMESTAMPTZ DEFAULT NOW();

-- 2. Copy data back from chat_message_texts
UPDATE chat_messages cm
SET message = cmt.message,
    from_actor_id = cmt.from_actor_id,
    sent_at = cmt.sent_at,
    channel = CASE
        WHEN cmt.discussion_id IS NOT NULL THEN 'discussion-' || cmt.discussion_id
        ELSE NULL
    END
FROM chat_message_texts cmt
WHERE cm.message_text_id = cmt.id;

-- 3. Make columns NOT NULL where needed
ALTER TABLE chat_messages ALTER COLUMN message SET NOT NULL;
ALTER TABLE chat_messages ALTER COLUMN from_actor_id SET NOT NULL;
ALTER TABLE chat_messages ALTER COLUMN sent_at SET NOT NULL;

-- 4. Drop the FK column and index
DROP INDEX IF EXISTS idx_cm_message_text;
ALTER TABLE chat_messages DROP COLUMN message_text_id;

-- 5. Drop the new table
DROP TABLE chat_message_texts;

COMMIT;
