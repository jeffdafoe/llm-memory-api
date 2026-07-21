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

-- Track when the passphrase was last rotated (surfaced in the admin agent list)
ALTER TABLE agents ADD COLUMN passphrase_rotated_at TIMESTAMPTZ;

-- Backfill existing active agents with an initial rotation timestamp
UPDATE agents SET passphrase_rotated_at = NOW() WHERE status = 'active';
