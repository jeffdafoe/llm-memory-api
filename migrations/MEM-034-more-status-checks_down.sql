-- MEM-034 rollback

ALTER TABLE agents DROP CONSTRAINT IF EXISTS chk_agents_status;
ALTER TABLE discussions DROP CONSTRAINT IF EXISTS chk_discussions_mode;
ALTER TABLE discussion_participants DROP CONSTRAINT IF EXISTS chk_discussion_participants_role;
