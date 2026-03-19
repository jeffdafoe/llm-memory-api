-- MEM-065 rollback

ALTER TABLE mail DROP COLUMN IF EXISTS in_reply_to;
