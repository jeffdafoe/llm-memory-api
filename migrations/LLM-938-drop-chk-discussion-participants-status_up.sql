-- discussion_participants has two CHECK constraints on status. The older
-- chk_discussion_participants_status does not allow 'deferred', so every
-- defer failed; discussion_participants_status_check (which allows it)
-- stays. The DROP takes an ACCESS EXCLUSIVE lock; if it cannot get it
-- within the lock_timeout the migration fails whole, and the next deploy
-- applies it again.
BEGIN;

SET LOCAL lock_timeout = '5s';

ALTER TABLE discussion_participants DROP CONSTRAINT IF EXISTS chk_discussion_participants_status;

COMMIT;
