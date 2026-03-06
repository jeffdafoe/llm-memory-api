-- MEM-033 rollback: Remove CHECK constraints from discussion tables

ALTER TABLE discussions DROP CONSTRAINT IF EXISTS chk_discussions_status;
ALTER TABLE discussion_participants DROP CONSTRAINT IF EXISTS chk_discussion_participants_status;
ALTER TABLE discussion_votes DROP CONSTRAINT IF EXISTS chk_discussion_votes_status;
ALTER TABLE discussion_votes DROP CONSTRAINT IF EXISTS chk_discussion_votes_type;
ALTER TABLE discussion_votes DROP CONSTRAINT IF EXISTS chk_discussion_votes_threshold;
