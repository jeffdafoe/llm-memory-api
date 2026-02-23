-- MEM-009: Rollback agent tokens

ALTER TABLE agents DROP COLUMN IF EXISTS token_hash;
ALTER TABLE agents DROP COLUMN IF EXISTS token_salt;
ALTER TABLE agents DROP COLUMN IF EXISTS status;
