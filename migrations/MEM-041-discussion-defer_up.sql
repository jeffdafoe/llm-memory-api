-- MEM-041: Discussion defer/table feature
-- Allows invited participants to defer a discussion for later without the timeout expiring.
-- Adds deferred_at and defer_count columns to discussion_participants,
-- updates the CHECK constraint to include 'deferred' status,
-- and adds config keys for defer timeout and max defer count.

-- Add columns for tracking deferrals
ALTER TABLE discussion_participants ADD COLUMN deferred_at TIMESTAMPTZ DEFAULT NULL;
ALTER TABLE discussion_participants ADD COLUMN defer_count INTEGER DEFAULT 0;

-- Update CHECK constraint to include 'deferred' as a valid participant status
ALTER TABLE discussion_participants DROP CONSTRAINT IF EXISTS discussion_participants_status_check;
ALTER TABLE discussion_participants ADD CONSTRAINT discussion_participants_status_check
    CHECK (status IN ('invited', 'joined', 'left', 'timed_out', 'deferred'));

-- Config keys for defer behavior
INSERT INTO config (key, value) VALUES ('discussion_defer_timeout', '1440');
INSERT INTO config (key, value) VALUES ('max_defer_count', '3');
