BEGIN;

DROP INDEX IF EXISTS idx_cmt_not_error;
ALTER TABLE chat_message_texts DROP COLUMN IF EXISTS is_error;

COMMIT;
