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

-- Bootstrap text appended to dream agent instructions when processing an agent's logs
INSERT INTO config (key, value) VALUES ('dream_bootstrap', 'Your account has nightly dream processing enabled. Each night, your conversation logs from the day are reviewed and consolidated into structured memory notes. These notes appear in your namespace under dreams/ and are available via semantic search in future sessions. The dream process extracts corrections, decisions, preferences, and context that should persist — things that came up naturally in conversation but might not have been explicitly saved. You do not need to do anything special during your sessions for this to work.');

-- Search tuning for dream notes
INSERT INTO config (key, value) VALUES ('search_decay_halflife_dream', '30');
INSERT INTO config (key, value) VALUES ('search_dream_weight', '1.0');
