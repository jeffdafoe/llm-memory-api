-- MEM-020: Remote MCP support
-- Adds documents table for note storage, agent API keys for MCP auth,
-- and a permissions system for tool-level access control.

-- Documents table: stores notes/content that gets auto-indexed into vector DB
CREATE TABLE documents (
    id SERIAL PRIMARY KEY,
    namespace VARCHAR(64) NOT NULL,
    slug VARCHAR(255) NOT NULL,
    title VARCHAR(500),
    content TEXT NOT NULL,
    created_at TIMESTAMPTZ DEFAULT NOW(),
    updated_at TIMESTAMPTZ DEFAULT NOW(),
    UNIQUE(namespace, slug)
);

CREATE INDEX idx_documents_namespace ON documents (namespace);

-- Agent API keys: per-agent keys for MCP HTTP auth (OAuth client credentials)
CREATE TABLE agent_api_keys (
    id SERIAL PRIMARY KEY,
    agent VARCHAR(50) NOT NULL REFERENCES agents(agent),
    key_hash VARCHAR(255) NOT NULL,
    key_salt VARCHAR(64) NOT NULL,
    label VARCHAR(100),
    created_at TIMESTAMPTZ DEFAULT NOW(),
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ
);

CREATE INDEX idx_agent_api_keys_agent ON agent_api_keys (agent);

-- Permissions: defines available permissions (one row per permission)
CREATE TABLE permissions (
    id SERIAL PRIMARY KEY,
    name VARCHAR(100) NOT NULL UNIQUE,
    created_at TIMESTAMPTZ DEFAULT NOW()
);

-- Agent permissions: which agent has which permissions (one row per grant)
CREATE TABLE agent_permissions (
    agent VARCHAR(50) NOT NULL REFERENCES agents(agent),
    permission_id INTEGER NOT NULL REFERENCES permissions(id),
    granted_at TIMESTAMPTZ DEFAULT NOW(),
    PRIMARY KEY (agent, permission_id)
);

-- Seed initial MCP tool permissions
INSERT INTO permissions (name) VALUES
    ('mcp_search'),
    ('mcp_save_note'),
    ('mcp_list_notes'),
    ('mcp_read_note'),
    ('mcp_delete_note');
