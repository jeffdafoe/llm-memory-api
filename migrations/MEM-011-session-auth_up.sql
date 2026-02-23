-- Session-based authentication for agents
-- Agents login with their passphrase, get a short-lived session token

CREATE TABLE agent_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent TEXT NOT NULL REFERENCES agents(agent),
    token_hash TEXT NOT NULL,
    token_salt TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_agent_sessions_agent ON agent_sessions(agent);
CREATE INDEX idx_agent_sessions_expires ON agent_sessions(expires_at);

-- Track when passphrase was last rotated so login can hint rotation_due
ALTER TABLE agents ADD COLUMN passphrase_rotated_at TIMESTAMPTZ;

-- Backfill existing agents so they don't immediately get rotation_due
UPDATE agents SET passphrase_rotated_at = NOW() WHERE status = 'active';
