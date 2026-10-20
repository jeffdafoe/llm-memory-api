-- MEM-152 down: recreate mcp_sessions in its post-MEM-050 shape (actor_id, no agent).

CREATE TABLE mcp_sessions (
    session_id TEXT PRIMARY KEY,
    tools_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor_id INTEGER NOT NULL,
    CONSTRAINT fk_mcp_actor FOREIGN KEY (actor_id) REFERENCES actors(id)
);

CREATE INDEX idx_mcp_sessions_actor ON mcp_sessions(actor_id);

GRANT ALL ON TABLE mcp_sessions TO memory_api;
GRANT SELECT, INSERT, DELETE, UPDATE ON TABLE mcp_sessions TO claude;
