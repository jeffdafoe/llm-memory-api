-- MEM-016: System error reporting table
-- Stores errors reported by agents/transports, with handler-driven resolution

CREATE TABLE system_errors (
    id serial PRIMARY KEY,
    agent text NOT NULL,
    source text NOT NULL,
    error_code text NOT NULL,
    context jsonb,
    status text NOT NULL DEFAULT 'unhandled',
    handler_action text,
    resolved_at timestamptz,
    reported_at timestamptz DEFAULT NOW()
);

CREATE INDEX idx_system_errors_agent ON system_errors (agent);
CREATE INDEX idx_system_errors_status ON system_errors (status);
CREATE INDEX idx_system_errors_error_code ON system_errors (error_code);
