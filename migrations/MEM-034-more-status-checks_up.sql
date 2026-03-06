-- MEM-034: Add CHECK constraints to remaining status/mode/role columns

ALTER TABLE agents
    ADD CONSTRAINT chk_agents_status
    CHECK (status IN ('active'));

ALTER TABLE discussions
    ADD CONSTRAINT chk_discussions_mode
    CHECK (mode IN ('realtime', 'async'));

ALTER TABLE discussion_participants
    ADD CONSTRAINT chk_discussion_participants_role
    CHECK (role IN ('required', 'optional'));
