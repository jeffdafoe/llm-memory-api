-- MEM-088: Add dream_mode to agent_configuration
-- Controls post-session conversation log analysis behavior per agent.
-- Values: 'none' (disabled), 'companion' (emotional/personal), 'technical' (work/code-focused)

ALTER TABLE agent_configuration ADD COLUMN dream_mode VARCHAR(20) NOT NULL DEFAULT 'none';

ALTER TABLE agent_configuration ADD CONSTRAINT chk_agent_configuration_dream_mode
    CHECK (dream_mode IN ('none', 'companion', 'technical'));

-- Tracks when this agent was last processed by the dream job
ALTER TABLE agent_configuration ADD COLUMN last_dream_at TIMESTAMPTZ;

-- Global switch for dream processing (cron job checks this before running)
INSERT INTO config (key, value) VALUES ('dream_processing_enabled', 'false');

-- Search tuning for dream notes
INSERT INTO config (key, value) VALUES ('search_decay_halflife_dream', '30');
INSERT INTO config (key, value) VALUES ('search_dream_weight', '1.0');
