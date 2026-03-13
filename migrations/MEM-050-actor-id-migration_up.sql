-- MEM-050: Actor ID migration
-- Introduces actors table with integer IDs. Migrates all agent name references
-- to actor_id foreign keys. Enables agent rename and user participation in messaging.

-- Step 1: Create actors table
CREATE TABLE actors (
    id SERIAL PRIMARY KEY,
    name VARCHAR(50) NOT NULL UNIQUE,
    type VARCHAR(20) NOT NULL DEFAULT 'agent'
        CHECK (type IN ('agent', 'user', 'system')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Step 2: Seed actors from existing agents
INSERT INTO actors (name, type, created_at)
SELECT agent,
       CASE WHEN agent = 'system' THEN 'system' ELSE 'agent' END,
       registered_at
FROM agents;

-- Step 3: Add actor_id to agents, backfill, make it the new PK
ALTER TABLE agents ADD COLUMN actor_id INTEGER;
UPDATE agents SET actor_id = (SELECT id FROM actors WHERE name = agents.agent);
ALTER TABLE agents ALTER COLUMN actor_id SET NOT NULL;
ALTER TABLE agents ADD CONSTRAINT fk_agents_actor FOREIGN KEY (actor_id) REFERENCES actors(id);
ALTER TABLE agents ADD CONSTRAINT uq_agents_actor_id UNIQUE (actor_id);

-- Step 4: Migrate referencing tables
-- Pattern: add column → backfill → set NOT NULL → add FK

-- 4a: chat_messages (from_agent, to_agent → from_actor_id, to_actor_id)
ALTER TABLE chat_messages ADD COLUMN from_actor_id INTEGER;
ALTER TABLE chat_messages ADD COLUMN to_actor_id INTEGER;
UPDATE chat_messages SET
    from_actor_id = (SELECT id FROM actors WHERE name = chat_messages.from_agent),
    to_actor_id = (SELECT id FROM actors WHERE name = chat_messages.to_agent);
ALTER TABLE chat_messages ALTER COLUMN from_actor_id SET NOT NULL;
ALTER TABLE chat_messages ALTER COLUMN to_actor_id SET NOT NULL;
ALTER TABLE chat_messages ADD CONSTRAINT fk_chat_from_actor FOREIGN KEY (from_actor_id) REFERENCES actors(id);
ALTER TABLE chat_messages ADD CONSTRAINT fk_chat_to_actor FOREIGN KEY (to_actor_id) REFERENCES actors(id);

-- 4b: mail (from_agent, to_agent → from_actor_id, to_actor_id)
ALTER TABLE mail ADD COLUMN from_actor_id INTEGER;
ALTER TABLE mail ADD COLUMN to_actor_id INTEGER;
UPDATE mail SET
    from_actor_id = (SELECT id FROM actors WHERE name = mail.from_agent),
    to_actor_id = (SELECT id FROM actors WHERE name = mail.to_agent);
ALTER TABLE mail ALTER COLUMN from_actor_id SET NOT NULL;
ALTER TABLE mail ALTER COLUMN to_actor_id SET NOT NULL;
ALTER TABLE mail ADD CONSTRAINT fk_mail_from_actor FOREIGN KEY (from_actor_id) REFERENCES actors(id);
ALTER TABLE mail ADD CONSTRAINT fk_mail_to_actor FOREIGN KEY (to_actor_id) REFERENCES actors(id);

-- 4c: discussions (created_by → created_by_actor_id)
ALTER TABLE discussions ADD COLUMN created_by_actor_id INTEGER;
UPDATE discussions SET created_by_actor_id = (SELECT id FROM actors WHERE name = discussions.created_by);
ALTER TABLE discussions ALTER COLUMN created_by_actor_id SET NOT NULL;
ALTER TABLE discussions ADD CONSTRAINT fk_discussions_created_by FOREIGN KEY (created_by_actor_id) REFERENCES actors(id);

-- 4d: discussion_participants (agent → actor_id)
-- Must drop PK first since agent is part of composite PK
ALTER TABLE discussion_participants DROP CONSTRAINT discussion_participants_pkey;
ALTER TABLE discussion_participants ADD COLUMN actor_id INTEGER;
UPDATE discussion_participants SET actor_id = (SELECT id FROM actors WHERE name = discussion_participants.agent);
ALTER TABLE discussion_participants ALTER COLUMN actor_id SET NOT NULL;
ALTER TABLE discussion_participants ADD CONSTRAINT fk_dp_actor FOREIGN KEY (actor_id) REFERENCES actors(id);

-- 4e: discussion_votes (proposed_by → proposed_by_actor_id)
ALTER TABLE discussion_votes ADD COLUMN proposed_by_actor_id INTEGER;
UPDATE discussion_votes SET proposed_by_actor_id = (SELECT id FROM actors WHERE name = discussion_votes.proposed_by);
ALTER TABLE discussion_votes ALTER COLUMN proposed_by_actor_id SET NOT NULL;
ALTER TABLE discussion_votes ADD CONSTRAINT fk_dv_proposed_by FOREIGN KEY (proposed_by_actor_id) REFERENCES actors(id);

-- 4f: discussion_ballots (agent → actor_id)
-- Must drop PK first since agent is part of composite PK
ALTER TABLE discussion_ballots DROP CONSTRAINT discussion_ballots_pkey;
ALTER TABLE discussion_ballots ADD COLUMN actor_id INTEGER;
UPDATE discussion_ballots SET actor_id = (SELECT id FROM actors WHERE name = discussion_ballots.agent);
ALTER TABLE discussion_ballots ALTER COLUMN actor_id SET NOT NULL;
ALTER TABLE discussion_ballots ADD CONSTRAINT fk_db_actor FOREIGN KEY (actor_id) REFERENCES actors(id);

-- 4g: agent_sessions (agent → actor_id)
ALTER TABLE agent_sessions DROP CONSTRAINT agent_sessions_agent_fkey;
ALTER TABLE agent_sessions ADD COLUMN actor_id INTEGER;
UPDATE agent_sessions SET actor_id = (SELECT id FROM actors WHERE name = agent_sessions.agent);
ALTER TABLE agent_sessions ALTER COLUMN actor_id SET NOT NULL;
ALTER TABLE agent_sessions ADD CONSTRAINT fk_as_actor FOREIGN KEY (actor_id) REFERENCES actors(id);

-- 4h: agent_api_keys (agent → actor_id)
ALTER TABLE agent_api_keys DROP CONSTRAINT agent_api_keys_agent_fkey;
ALTER TABLE agent_api_keys ADD COLUMN actor_id INTEGER;
UPDATE agent_api_keys SET actor_id = (SELECT id FROM actors WHERE name = agent_api_keys.agent);
ALTER TABLE agent_api_keys ALTER COLUMN actor_id SET NOT NULL;
ALTER TABLE agent_api_keys ADD CONSTRAINT fk_aak_actor FOREIGN KEY (actor_id) REFERENCES actors(id);

-- 4i: agent_permissions (agent → actor_id)
-- Must drop PK first since agent is part of composite PK
ALTER TABLE agent_permissions DROP CONSTRAINT agent_permissions_agent_fkey;
ALTER TABLE agent_permissions DROP CONSTRAINT agent_permissions_pkey;
ALTER TABLE agent_permissions ADD COLUMN actor_id INTEGER;
UPDATE agent_permissions SET actor_id = (SELECT id FROM actors WHERE name = agent_permissions.agent);
ALTER TABLE agent_permissions ALTER COLUMN actor_id SET NOT NULL;
ALTER TABLE agent_permissions ADD CONSTRAINT fk_ap_actor FOREIGN KEY (actor_id) REFERENCES actors(id);

-- 4j: mcp_sessions (agent → actor_id)
ALTER TABLE mcp_sessions DROP CONSTRAINT mcp_sessions_agent_fkey;
ALTER TABLE mcp_sessions ADD COLUMN actor_id INTEGER;
UPDATE mcp_sessions SET actor_id = (SELECT id FROM actors WHERE name = mcp_sessions.agent);
ALTER TABLE mcp_sessions ALTER COLUMN actor_id SET NOT NULL;
ALTER TABLE mcp_sessions ADD CONSTRAINT fk_mcp_actor FOREIGN KEY (actor_id) REFERENCES actors(id);

-- 4k: system_errors (agent → actor_id, nullable)
ALTER TABLE system_errors ADD COLUMN actor_id INTEGER;
UPDATE system_errors SET actor_id = (SELECT id FROM actors WHERE name = system_errors.agent)
WHERE agent IS NOT NULL;
-- actor_id stays nullable — old errors might reference unknown agents

-- 4l: documents (created_by → created_by_actor_id, nullable)
ALTER TABLE documents DROP CONSTRAINT documents_created_by_fkey;
ALTER TABLE documents ADD COLUMN created_by_actor_id INTEGER;
UPDATE documents SET created_by_actor_id = (SELECT id FROM actors WHERE name = documents.created_by)
WHERE created_by IS NOT NULL;
ALTER TABLE documents ADD CONSTRAINT fk_docs_created_by FOREIGN KEY (created_by_actor_id) REFERENCES actors(id);
-- created_by_actor_id stays nullable

-- 4m: virtual_agent_usage (agent → actor_id)
ALTER TABLE virtual_agent_usage DROP CONSTRAINT virtual_agent_usage_agent_fkey;
ALTER TABLE virtual_agent_usage ADD COLUMN actor_id INTEGER;
UPDATE virtual_agent_usage SET actor_id = (SELECT id FROM actors WHERE name = virtual_agent_usage.agent);
ALTER TABLE virtual_agent_usage ALTER COLUMN actor_id SET NOT NULL;
ALTER TABLE virtual_agent_usage ADD CONSTRAINT fk_vau_actor FOREIGN KEY (actor_id) REFERENCES actors(id);

-- 4n: error_log (agent → actor_id, nullable)
ALTER TABLE error_log ADD COLUMN actor_id INTEGER;
UPDATE error_log SET actor_id = (SELECT id FROM actors WHERE name = error_log.agent)
WHERE agent IS NOT NULL;
-- actor_id stays nullable

-- 4o: request_log (agent → actor_id, nullable)
ALTER TABLE request_log ADD COLUMN actor_id INTEGER;
UPDATE request_log SET actor_id = (SELECT id FROM actors WHERE name = request_log.agent)
WHERE agent IS NOT NULL;
-- actor_id stays nullable — not all requests are authenticated

-- Step 5: Drop old name columns

-- Drop old indexes first (ones that reference old columns)
DROP INDEX IF EXISTS idx_chat_messages_to_agent;
DROP INDEX IF EXISTS idx_chat_messages_unacked;
DROP INDEX IF EXISTS idx_mail_to_agent_acked;
DROP INDEX IF EXISTS idx_discussion_participants_agent;
DROP INDEX IF EXISTS idx_agent_sessions_agent;
DROP INDEX IF EXISTS idx_agent_api_keys_agent;
DROP INDEX IF EXISTS idx_system_errors_agent;
DROP INDEX IF EXISTS idx_mcp_sessions_agent;
DROP INDEX IF EXISTS idx_va_usage_agent_date;
DROP INDEX IF EXISTS idx_error_log_agent;

-- Drop old agent name columns
ALTER TABLE chat_messages DROP COLUMN from_agent;
ALTER TABLE chat_messages DROP COLUMN to_agent;
ALTER TABLE mail DROP COLUMN from_agent;
ALTER TABLE mail DROP COLUMN to_agent;
ALTER TABLE discussions DROP COLUMN created_by;
ALTER TABLE discussion_participants DROP COLUMN agent;
ALTER TABLE discussion_votes DROP COLUMN proposed_by;
ALTER TABLE discussion_ballots DROP COLUMN agent;
ALTER TABLE agent_sessions DROP COLUMN agent;
ALTER TABLE agent_api_keys DROP COLUMN agent;
ALTER TABLE agent_permissions DROP COLUMN agent;
ALTER TABLE mcp_sessions DROP COLUMN agent;
ALTER TABLE system_errors DROP COLUMN agent;
ALTER TABLE documents DROP COLUMN created_by;
ALTER TABLE virtual_agent_usage DROP COLUMN agent;
ALTER TABLE error_log DROP COLUMN agent;
ALTER TABLE request_log DROP COLUMN agent;

-- Drop old PK on agents, set actor_id as new PK
ALTER TABLE agents DROP CONSTRAINT agents_pkey;
ALTER TABLE agents DROP COLUMN agent;
ALTER TABLE agents ADD PRIMARY KEY (actor_id);

-- Step 6: Recreate indexes on new columns
CREATE INDEX idx_chat_messages_to_actor ON chat_messages (to_actor_id, id);
CREATE INDEX idx_chat_messages_unacked ON chat_messages (to_actor_id, channel) WHERE acked_at IS NULL;
CREATE INDEX idx_mail_to_actor_acked ON mail (to_actor_id, acked_at);
CREATE INDEX idx_discussion_participants_actor ON discussion_participants (actor_id, status);
CREATE INDEX idx_agent_sessions_actor ON agent_sessions (actor_id);
CREATE INDEX idx_agent_api_keys_actor ON agent_api_keys (actor_id);
CREATE INDEX idx_system_errors_actor ON system_errors (actor_id) WHERE actor_id IS NOT NULL;
CREATE INDEX idx_mcp_sessions_actor ON mcp_sessions (actor_id);
CREATE INDEX idx_va_usage_actor_date ON virtual_agent_usage (actor_id, created_at);
CREATE INDEX idx_error_log_actor ON error_log (actor_id) WHERE actor_id IS NOT NULL;

-- Step 7: Recreate composite PKs
ALTER TABLE discussion_participants ADD PRIMARY KEY (discussion_id, actor_id);
ALTER TABLE discussion_ballots ADD PRIMARY KEY (vote_id, actor_id);
ALTER TABLE agent_permissions ADD PRIMARY KEY (actor_id, permission_id);

-- Step 8: Recreate agent_status view with actor join
DROP VIEW IF EXISTS agent_status;
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
