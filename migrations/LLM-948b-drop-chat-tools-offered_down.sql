-- Reverts LLM-948b. The column comes back empty; the dropped values are
-- gone. The code does not write it, so it stays NULL.
BEGIN;

SET LOCAL lock_timeout = '5s';

ALTER TABLE chat_message_texts ADD COLUMN IF NOT EXISTS tools_offered jsonb;

COMMIT;
