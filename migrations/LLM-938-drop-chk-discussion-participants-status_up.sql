-- discussion_participants has two CHECK constraints on status. The older
-- chk_discussion_participants_status does not allow 'deferred', so every
-- defer failed; discussion_participants_status_check (which allows it)
-- stays.
BEGIN;

ALTER TABLE discussion_participants DROP CONSTRAINT IF EXISTS chk_discussion_participants_status;

COMMIT;
