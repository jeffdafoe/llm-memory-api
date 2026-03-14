-- MEM-061: Add soft-delete support to chat_messages (matches mail pattern)
ALTER TABLE chat_messages ADD COLUMN deleted_at TIMESTAMPTZ DEFAULT NULL;
