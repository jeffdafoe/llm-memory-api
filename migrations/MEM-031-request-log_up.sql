-- MEM-031: Persistent API request log
-- Replaces the in-memory ring buffer so request history survives restarts.

CREATE TABLE request_log (
    id SERIAL PRIMARY KEY,
    timestamp TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    method VARCHAR(10) NOT NULL,
    path TEXT,
    status INTEGER,
    duration_ms INTEGER,
    agent VARCHAR(100),
    ip VARCHAR(45),
    request_length INTEGER
);

CREATE INDEX idx_request_log_timestamp ON request_log (timestamp);
