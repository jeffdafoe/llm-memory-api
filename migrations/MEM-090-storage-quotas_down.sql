ALTER TABLE agent_configuration DROP COLUMN IF EXISTS storage_quota;
DELETE FROM config WHERE key = 'default_storage_quota';
