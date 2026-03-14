-- MEM-061: Remove soft-delete from chat_messages
ALTER TABLE chat_messages DROP COLUMN IF EXISTS deleted_at;
