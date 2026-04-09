-- MEM-109 rollback

BEGIN;

DROP INDEX IF EXISTS idx_actors_realms;
DELETE FROM config WHERE key = 'realm_host_map';
ALTER TABLE invite_codes DROP COLUMN IF EXISTS realm;
ALTER TABLE actors DROP COLUMN IF EXISTS realms;

COMMIT;
