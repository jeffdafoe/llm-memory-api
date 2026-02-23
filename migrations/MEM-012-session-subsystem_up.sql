-- MEM-012: Add subsystem label to sessions for observability
-- Allows status endpoint to show which subsystems are connected per agent

ALTER TABLE agent_sessions ADD COLUMN subsystem TEXT;
