-- MEM-023: Persistent MCP session tracking.
-- Stores session IDs in the database so they survive Node restarts.
-- tools_hash detects when tool definitions change between deploys,
-- preventing stale sessions from being rehydrated with mismatched tools.

CREATE TABLE mcp_sessions (
    session_id TEXT PRIMARY KEY,
    agent TEXT NOT NULL REFERENCES agents(agent),
    tools_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_mcp_sessions_agent ON mcp_sessions(agent);
