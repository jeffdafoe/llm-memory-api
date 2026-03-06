-- MEM-033: Add CHECK constraints to discussion status columns
-- Enforces valid status values for discussions and discussion_participants.

ALTER TABLE discussions
    ADD CONSTRAINT chk_discussions_status
    CHECK (status IN ('waiting', 'active', 'concluded', 'cancelled', 'timed_out'));

ALTER TABLE discussion_participants
    ADD CONSTRAINT chk_discussion_participants_status
    CHECK (status IN ('invited', 'joined', 'left', 'timed_out'));

ALTER TABLE discussion_votes
    ADD CONSTRAINT chk_discussion_votes_status
    CHECK (status IN ('open', 'closed'));

ALTER TABLE discussion_votes
    ADD CONSTRAINT chk_discussion_votes_type
    CHECK (type IN ('general', 'conclude'));

ALTER TABLE discussion_votes
    ADD CONSTRAINT chk_discussion_votes_threshold
    CHECK (threshold IN ('unanimous', 'majority'));
