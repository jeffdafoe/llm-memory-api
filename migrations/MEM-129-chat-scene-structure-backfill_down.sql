-- MEM-129 down: cannot reliably reverse the backfill — the original
-- NULL state is unrecoverable since we don't preserve which rows were
-- inherited vs. originally set. No-op down. If a true rollback of the
-- backfill is needed, restore from the pre-migration database snapshot.

BEGIN;
-- intentionally empty
COMMIT;
