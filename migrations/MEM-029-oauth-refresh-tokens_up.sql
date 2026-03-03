-- MEM-029: OAuth refresh tokens.
-- Stores opaque refresh tokens so claude.ai can silently re-authenticate
-- when the 1-hour access token (JWT) expires. Without this, the MCP
-- connector disconnects after an hour with no way to recover.

CREATE TABLE oauth_refresh_tokens (
    id SERIAL PRIMARY KEY,
    token_hash VARCHAR(255) NOT NULL,
    token_salt VARCHAR(64) NOT NULL,
    agent VARCHAR(50) NOT NULL REFERENCES agents(agent),
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    revoked_at TIMESTAMPTZ
);

CREATE INDEX idx_oauth_refresh_tokens_agent ON oauth_refresh_tokens (agent);
