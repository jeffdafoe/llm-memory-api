-- MEM-041 rollback: Remove discussion defer/table feature

-- Remove config keys
DELETE FROM config WHERE key IN ('discussion_defer_timeout', 'max_defer_count');

-- Restore original CHECK constraint without 'deferred'
ALTER TABLE discussion_participants DROP CONSTRAINT IF EXISTS discussion_participants_status_check;
ALTER TABLE discussion_participants ADD CONSTRAINT discussion_participants_status_check
    CHECK (status IN ('invited', 'joined', 'left', 'timed_out'));

-- Remove columns
ALTER TABLE discussion_participants DROP COLUMN IF EXISTS deferred_at;
ALTER TABLE discussion_participants DROP COLUMN IF EXISTS defer_count;
