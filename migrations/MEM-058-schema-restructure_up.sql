-- MEM-058: Schema Restructure
-- Actors becomes the identity + auth + presence table.
-- Agents is renamed to agent_configuration (AI-specific config only).
-- Users table absorbed into actors.
-- agent_sessions + user_sessions merged into unified sessions table.

-- ============================================================
-- Step 1: Drop the agent_status view (references agents columns we're moving)
-- ============================================================

DROP VIEW IF EXISTS agent_status;

-- ============================================================
-- Step 2: Add new columns to actors
-- ============================================================

-- Agent passphrase credentials (from agents.token_hash/token_salt)
ALTER TABLE actors ADD COLUMN token_hash VARCHAR(128);
ALTER TABLE actors ADD COLUMN token_salt VARCHAR(64);

-- Web UI credentials (from users.password_hash/password_salt)
ALTER TABLE actors ADD COLUMN password_hash TEXT;
ALTER TABLE actors ADD COLUMN password_salt TEXT;

-- Presence and status fields (from agents)
ALTER TABLE actors ADD COLUMN passphrase_rotated_at TIMESTAMPTZ;
ALTER TABLE actors ADD COLUMN status VARCHAR(20) NOT NULL DEFAULT 'active';
ALTER TABLE actors ADD COLUMN last_seen TIMESTAMPTZ;
ALTER TABLE actors ADD COLUMN active_since TIMESTAMPTZ;
ALTER TABLE actors ADD COLUMN expertise TEXT NOT NULL DEFAULT '[]';

-- ============================================================
-- Step 3: Copy data from agents and users into actors
-- ============================================================

-- Copy agent credentials and presence fields
UPDATE actors ac SET
    token_hash = a.token_hash,
    token_salt = a.token_salt,
    passphrase_rotated_at = a.passphrase_rotated_at,
    status = a.status,
    last_seen = a.last_seen,
    active_since = a.active_since,
    expertise = a.expertise
FROM agents a
WHERE a.actor_id = ac.id;

-- Create actor rows for users that don't already have one
INSERT INTO actors (name, type, password_hash, password_salt)
SELECT u.username, 'user', u.password_hash, u.password_salt
FROM users u
WHERE NOT EXISTS (SELECT 1 FROM actors ac WHERE ac.name = u.username);

-- Copy user credentials into actors for users that already have an actor row
-- (handles case where a user name matches an existing agent actor)
UPDATE actors ac SET
    password_hash = u.password_hash,
    password_salt = u.password_salt
FROM users u
WHERE ac.name = u.username;

-- ============================================================
-- Step 4: Create unified sessions table
-- ============================================================

CREATE TABLE sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id INTEGER NOT NULL REFERENCES actors(id),
    token_hash TEXT NOT NULL,
    token_salt TEXT NOT NULL,
    kind VARCHAR(10) NOT NULL CHECK (kind IN ('web', 'api')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    subsystem TEXT
);

CREATE INDEX idx_sessions_actor ON sessions (actor_id);
CREATE INDEX idx_sessions_expires ON sessions (expires_at);

-- Migrate active agent sessions (already hashed)
INSERT INTO sessions (id, actor_id, token_hash, token_salt, kind, created_at, expires_at, subsystem)
SELECT id, actor_id, token_hash, token_salt, 'api', created_at, expires_at, subsystem
FROM agent_sessions
WHERE expires_at > NOW();

-- Do NOT migrate user sessions — they use plaintext tokens.
-- Users will need to re-login after this migration.

-- ============================================================
-- Step 5: Rename agents to agent_configuration, clean up columns
-- ============================================================

ALTER TABLE agents RENAME TO agent_configuration;

-- Drop credential/presence columns that moved to actors
ALTER TABLE agent_configuration DROP COLUMN token_hash;
ALTER TABLE agent_configuration DROP COLUMN token_salt;
ALTER TABLE agent_configuration DROP COLUMN passphrase_rotated_at;
ALTER TABLE agent_configuration DROP COLUMN status;
ALTER TABLE agent_configuration DROP COLUMN last_seen;
ALTER TABLE agent_configuration DROP COLUMN active_since;
ALTER TABLE agent_configuration DROP COLUMN expertise;
ALTER TABLE agent_configuration DROP COLUMN registered_at;

-- Drop status constraint (status column moved to actors)
ALTER TABLE agent_configuration DROP CONSTRAINT IF EXISTS chk_agents_status;

-- Rename remaining constraints to match new table name
ALTER TABLE agent_configuration RENAME CONSTRAINT agents_pkey TO agent_configuration_pkey;
ALTER TABLE agent_configuration RENAME CONSTRAINT fk_agents_actor TO fk_agent_configuration_actor;
ALTER TABLE agent_configuration RENAME CONSTRAINT uq_agents_actor_id TO uq_agent_configuration_actor_id;
ALTER TABLE agent_configuration RENAME CONSTRAINT chk_agents_provider TO chk_agent_configuration_provider;

-- Add status constraint on actors
ALTER TABLE actors ADD CONSTRAINT chk_actors_status CHECK (status IN ('active'));

-- ============================================================
-- Step 6: Drop old tables
-- ============================================================

-- user_sessions FKs to users, so drop it first
DROP TABLE user_sessions;
DROP TABLE users;
DROP TABLE agent_sessions;

-- ============================================================
-- Step 7: Drop the type column from actors
-- ============================================================
-- Actor identity is capability-based: has agent_configuration row = agent,
-- has password_hash = web user, has both = dual identity (e.g. Wendy).
-- The type column is redundant and eliminated.

ALTER TABLE actors DROP COLUMN type;

-- ============================================================
-- Step 8: Recreate agent_status view
-- ============================================================

CREATE VIEW agent_status AS
SELECT ac.id AS actor_id,
       ac.name AS agent,
       CASE
           WHEN agc.virtual = TRUE THEN 'available'
           WHEN ac.last_seen > NOW() - INTERVAL '15 minutes' THEN 'online'
           WHEN ac.last_seen IS NOT NULL THEN 'offline'
           ELSE 'unknown'
       END AS status,
       ac.last_seen,
       ac.passphrase_rotated_at,
       ac.created_at AS registered_at,
       ac.expertise,
       agc.provider,
       agc.model,
       agc.virtual,
       agc.personality,
       agc.cost_budget_daily,
       agc.cost_budget_monthly,
       CASE
           WHEN ac.active_since IS NOT NULL AND ac.active_since > NOW() - INTERVAL '30 minutes' THEN ac.active_since
           ELSE NULL
       END AS active_since,
       agc.cache_prompts,
       agc.learning_enabled,
       agc.max_tokens,
       agc.temperature
FROM actors ac
JOIN agent_configuration agc ON agc.actor_id = ac.id;
