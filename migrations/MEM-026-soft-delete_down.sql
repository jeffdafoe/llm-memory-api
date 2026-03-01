-- MEM-026 rollback: Remove soft delete support
ALTER TABLE documents DROP COLUMN deleted_at;
