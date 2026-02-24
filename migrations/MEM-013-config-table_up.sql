-- MEM-013: Configuration table
-- Key/value store for system-wide settings.

CREATE TABLE config (
    key VARCHAR(100) PRIMARY KEY,
    value TEXT NOT NULL,
    description TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO config (key, value, description) VALUES
    ('discussion_wait_timeout', '5', 'Minutes to wait for all participants before starting a discussion');

GRANT SELECT, INSERT, UPDATE, DELETE ON config TO memory_api;
