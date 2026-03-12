-- MEM-055: Create error_log table for structured error tracking.
-- Captures virtual-agent failures, provider errors, and other system errors
-- with enough context to diagnose issues from the admin dashboard.

CREATE TABLE error_log (
    id          SERIAL PRIMARY KEY,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    subsystem   TEXT NOT NULL,           -- e.g. 'virtual-agent', 'mail', 'discussion'
    action      TEXT NOT NULL,           -- e.g. 'direct-mail-error', 'api-call-failed'
    agent       TEXT,                    -- which agent was involved (nullable)
    context     TEXT,                    -- 'mail', 'chat', 'discussion' (nullable)
    context_id  TEXT,                    -- mail UUID, discussion ID, etc. (nullable)
    error_message TEXT NOT NULL,         -- short error description
    error_detail  TEXT                   -- full stack trace or extended info (nullable)
);

CREATE INDEX idx_error_log_created ON error_log (created_at DESC);
CREATE INDEX idx_error_log_subsystem ON error_log (subsystem);
CREATE INDEX idx_error_log_agent ON error_log (agent) WHERE agent IS NOT NULL;
