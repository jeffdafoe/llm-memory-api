-- MEM-030 rollback: Remove soft delete column from mail

ALTER TABLE mail DROP COLUMN IF EXISTS deleted_at;
