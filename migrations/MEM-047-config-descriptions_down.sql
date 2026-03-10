-- MEM-047 rollback: Clear backfilled descriptions (except the one from MEM-013).
UPDATE config SET description = NULL WHERE key != 'discussion_wait_timeout';
