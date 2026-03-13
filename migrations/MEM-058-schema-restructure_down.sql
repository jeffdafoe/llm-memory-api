-- MEM-058 rollback: Restore agents table, users table, separate session tables.
-- WARNING: User sessions and user table data cannot be fully restored —
-- user passwords were migrated to actors and the users table was dropped.
-- This rollback recreates the structure but users will need re-seeding.

-- ============================================================
-- Step 1: Drop the new agent_status view
-- ============================================================

DROP VIEW IF EXISTS agent_status;

-- ============================================================
-- Step 2: Recreate users and session tables
-- ============================================================

CREATE TABLE users (
    id SERIAL PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    password_salt TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_login TIMESTAMPTZ
);

CREATE TABLE user_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id INTEGER NOT NULL REFERENCES users(id),
    session_token TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_user_sessions_expires ON user_sessions (expires_at);
CREATE INDEX idx_user_sessions_token ON user_sessions (session_token);
CREATE INDEX idx_user_sessions_user_id ON user_sessions (user_id);

CREATE TABLE agent_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash TEXT NOT NULL,
    token_salt TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    subsystem TEXT,
    actor_id INTEGER NOT NULL REFERENCES actors(id)
);

CREATE INDEX idx_agent_sessions_actor ON agent_sessions (actor_id);
CREATE INDEX idx_agent_sessions_expires ON agent_sessions (expires_at);

-- Restore users from actors (best effort — only actors with password_hash)
INSERT INTO users (username, password_hash, password_salt, created_at)
SELECT name, password_hash, password_salt, created_at
FROM actors
WHERE password_hash IS NOT NULL;

-- Migrate active sessions back to agent_sessions
INSERT INTO agent_sessions (id, actor_id, token_hash, token_salt, created_at, expires_at, subsystem)
SELECT id, actor_id, token_hash, token_salt, created_at, expires_at, subsystem
FROM sessions
WHERE kind = 'api' AND expires_at > NOW();

-- ============================================================
-- Step 3: Rename agent_configuration back to agents, restore columns
-- ============================================================

-- Rename constraints back
ALTER TABLE agent_configuration RENAME CONSTRAINT agent_configuration_pkey TO agents_pkey;
ALTER TABLE agent_configuration RENAME CONSTRAINT fk_agent_configuration_actor TO fk_agents_actor;
ALTER TABLE agent_configuration RENAME CONSTRAINT uq_agent_configuration_actor_id TO uq_agents_actor_id;
ALTER TABLE agent_configuration RENAME CONSTRAINT chk_agent_configuration_provider TO chk_agents_provider;

ALTER TABLE agent_configuration RENAME TO agents;

-- Restore columns that were moved to actors
ALTER TABLE agents ADD COLUMN registered_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
ALTER TABLE agents ADD COLUMN token_hash VARCHAR(128);
ALTER TABLE agents ADD COLUMN token_salt VARCHAR(64);
ALTER TABLE agents ADD COLUMN passphrase_rotated_at TIMESTAMPTZ;
ALTER TABLE agents ADD COLUMN status VARCHAR(20) NOT NULL DEFAULT 'active';
ALTER TABLE agents ADD COLUMN last_seen TIMESTAMPTZ;
ALTER TABLE agents ADD COLUMN active_since TIMESTAMPTZ;
ALTER TABLE agents ADD COLUMN expertise TEXT NOT NULL DEFAULT '[]';

-- Copy data back from actors
UPDATE agents a SET
    token_hash = ac.token_hash,
    token_salt = ac.token_salt,
    passphrase_rotated_at = ac.passphrase_rotated_at,
    status = ac.status,
    last_seen = ac.last_seen,
    active_since = ac.active_since,
    expertise = ac.expertise,
    registered_at = ac.created_at
FROM actors ac
WHERE a.actor_id = ac.id;

-- Restore status constraint on agents
ALTER TABLE agents ADD CONSTRAINT chk_agents_status CHECK (status IN ('active'));

-- ============================================================
-- Step 4: Drop new tables and columns from actors, restore type column
-- ============================================================

DROP TABLE sessions;

-- Restore the type column (dropped in MEM-058 forward migration)
ALTER TABLE actors ADD COLUMN type VARCHAR(20) NOT NULL DEFAULT 'agent';

-- Set type based on what's attached:
-- actors with agent_configuration rows = 'agent', others = 'user'
UPDATE actors SET type = 'user'
WHERE id NOT IN (SELECT actor_id FROM agents);

ALTER TABLE actors DROP CONSTRAINT IF EXISTS chk_actors_status;
ALTER TABLE actors DROP COLUMN token_hash;
ALTER TABLE actors DROP COLUMN token_salt;
ALTER TABLE actors DROP COLUMN password_hash;
ALTER TABLE actors DROP COLUMN password_salt;
ALTER TABLE actors DROP COLUMN passphrase_rotated_at;
ALTER TABLE actors DROP COLUMN status;
ALTER TABLE actors DROP COLUMN last_seen;
ALTER TABLE actors DROP COLUMN active_since;
ALTER TABLE actors DROP COLUMN expertise;

-- ============================================================
-- Step 5: Recreate original agent_status view
-- ============================================================

CREATE VIEW agent_status AS
SELECT ac.id AS actor_id,
       ac.name AS agent,
       ac.type AS actor_type,
       CASE
           WHEN a.virtual = TRUE THEN 'available'
           WHEN a.last_seen > NOW() - INTERVAL '15 minutes' THEN 'online'
           WHEN a.last_seen IS NOT NULL THEN 'offline'
           ELSE 'unknown'
       END AS status,
       a.last_seen,
       a.passphrase_rotated_at,
       ac.created_at AS registered_at,
       a.expertise,
       a.provider,
       a.model,
       a.virtual,
       a.personality,
       a.cost_budget_daily,
       a.cost_budget_monthly,
       CASE
           WHEN a.active_since IS NOT NULL AND a.active_since > NOW() - INTERVAL '30 minutes' THEN a.active_since
           ELSE NULL
       END AS active_since,
       a.cache_prompts,
       a.learning_enabled,
       a.max_tokens,
       a.temperature
FROM actors ac
JOIN agents a ON a.actor_id = ac.id;
