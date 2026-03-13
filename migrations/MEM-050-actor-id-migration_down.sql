-- MEM-050 rollback: Restore agent name strings as identifiers
-- WARNING: This is a destructive rollback. If new actors were created
-- after migration (type='user'), they will be lost.

-- Drop the view first (depends on both tables)
DROP VIEW IF EXISTS agent_status;

-- Restore agent name column on agents table
ALTER TABLE agents DROP CONSTRAINT agents_pkey;
ALTER TABLE agents ADD COLUMN agent VARCHAR(50);
UPDATE agents SET agent = (SELECT name FROM actors WHERE id = agents.actor_id);
ALTER TABLE agents ALTER COLUMN agent SET NOT NULL;
ALTER TABLE agents ADD PRIMARY KEY (agent);

-- Drop new composite PKs before restoring columns
ALTER TABLE discussion_participants DROP CONSTRAINT discussion_participants_pkey;
ALTER TABLE discussion_ballots DROP CONSTRAINT discussion_ballots_pkey;
ALTER TABLE agent_permissions DROP CONSTRAINT agent_permissions_pkey;

-- Drop new indexes
DROP INDEX IF EXISTS idx_chat_messages_to_actor;
DROP INDEX IF EXISTS idx_chat_messages_unacked;
DROP INDEX IF EXISTS idx_mail_to_actor_acked;
DROP INDEX IF EXISTS idx_discussion_participants_actor;
DROP INDEX IF EXISTS idx_agent_sessions_actor;
DROP INDEX IF EXISTS idx_agent_api_keys_actor;
DROP INDEX IF EXISTS idx_system_errors_actor;
DROP INDEX IF EXISTS idx_mcp_sessions_actor;
DROP INDEX IF EXISTS idx_va_usage_actor_date;
DROP INDEX IF EXISTS idx_error_log_actor;

-- Restore name columns on all tables, backfill from actors
ALTER TABLE chat_messages ADD COLUMN from_agent VARCHAR(50);
ALTER TABLE chat_messages ADD COLUMN to_agent VARCHAR(50);
UPDATE chat_messages SET
    from_agent = (SELECT name FROM actors WHERE id = chat_messages.from_actor_id),
    to_agent = (SELECT name FROM actors WHERE id = chat_messages.to_actor_id);
ALTER TABLE chat_messages ALTER COLUMN from_agent SET NOT NULL;
ALTER TABLE chat_messages ALTER COLUMN to_agent SET NOT NULL;

ALTER TABLE mail ADD COLUMN from_agent VARCHAR(50);
ALTER TABLE mail ADD COLUMN to_agent VARCHAR(50);
UPDATE mail SET
    from_agent = (SELECT name FROM actors WHERE id = mail.from_actor_id),
    to_agent = (SELECT name FROM actors WHERE id = mail.to_actor_id);
ALTER TABLE mail ALTER COLUMN from_agent SET NOT NULL;
ALTER TABLE mail ALTER COLUMN to_agent SET NOT NULL;

ALTER TABLE discussions ADD COLUMN created_by VARCHAR(50);
UPDATE discussions SET created_by = (SELECT name FROM actors WHERE id = discussions.created_by_actor_id);
ALTER TABLE discussions ALTER COLUMN created_by SET NOT NULL;

ALTER TABLE discussion_participants ADD COLUMN agent VARCHAR(50);
UPDATE discussion_participants SET agent = (SELECT name FROM actors WHERE id = discussion_participants.actor_id);
ALTER TABLE discussion_participants ALTER COLUMN agent SET NOT NULL;

ALTER TABLE discussion_votes ADD COLUMN proposed_by VARCHAR(50);
UPDATE discussion_votes SET proposed_by = (SELECT name FROM actors WHERE id = discussion_votes.proposed_by_actor_id);
ALTER TABLE discussion_votes ALTER COLUMN proposed_by SET NOT NULL;

ALTER TABLE discussion_ballots ADD COLUMN agent VARCHAR(50);
UPDATE discussion_ballots SET agent = (SELECT name FROM actors WHERE id = discussion_ballots.actor_id);
ALTER TABLE discussion_ballots ALTER COLUMN agent SET NOT NULL;

ALTER TABLE agent_sessions ADD COLUMN agent TEXT;
UPDATE agent_sessions SET agent = (SELECT name FROM actors WHERE id = agent_sessions.actor_id);
ALTER TABLE agent_sessions ALTER COLUMN agent SET NOT NULL;

ALTER TABLE agent_api_keys ADD COLUMN agent VARCHAR(50);
UPDATE agent_api_keys SET agent = (SELECT name FROM actors WHERE id = agent_api_keys.actor_id);
ALTER TABLE agent_api_keys ALTER COLUMN agent SET NOT NULL;

ALTER TABLE agent_permissions ADD COLUMN agent VARCHAR(50);
UPDATE agent_permissions SET agent = (SELECT name FROM actors WHERE id = agent_permissions.actor_id);
ALTER TABLE agent_permissions ALTER COLUMN agent SET NOT NULL;

ALTER TABLE mcp_sessions ADD COLUMN agent TEXT;
UPDATE mcp_sessions SET agent = (SELECT name FROM actors WHERE id = mcp_sessions.actor_id);
ALTER TABLE mcp_sessions ALTER COLUMN agent SET NOT NULL;

ALTER TABLE system_errors ADD COLUMN agent TEXT;
UPDATE system_errors SET agent = (SELECT name FROM actors WHERE id = system_errors.actor_id)
WHERE actor_id IS NOT NULL;

ALTER TABLE documents ADD COLUMN created_by VARCHAR(50);
UPDATE documents SET created_by = (SELECT name FROM actors WHERE id = documents.created_by_actor_id)
WHERE created_by_actor_id IS NOT NULL;

ALTER TABLE virtual_agent_usage ADD COLUMN agent VARCHAR(50);
UPDATE virtual_agent_usage SET agent = (SELECT name FROM actors WHERE id = virtual_agent_usage.actor_id);
ALTER TABLE virtual_agent_usage ALTER COLUMN agent SET NOT NULL;

ALTER TABLE error_log ADD COLUMN agent TEXT;
UPDATE error_log SET agent = (SELECT name FROM actors WHERE id = error_log.actor_id)
WHERE actor_id IS NOT NULL;

ALTER TABLE request_log ADD COLUMN agent VARCHAR(100);
UPDATE request_log SET agent = (SELECT name FROM actors WHERE id = request_log.actor_id)
WHERE actor_id IS NOT NULL;

-- Drop actor_id columns and FKs from all tables
ALTER TABLE chat_messages DROP COLUMN from_actor_id;
ALTER TABLE chat_messages DROP COLUMN to_actor_id;
ALTER TABLE mail DROP COLUMN from_actor_id;
ALTER TABLE mail DROP COLUMN to_actor_id;
ALTER TABLE discussions DROP COLUMN created_by_actor_id;
ALTER TABLE discussion_participants DROP COLUMN actor_id;
ALTER TABLE discussion_votes DROP COLUMN proposed_by_actor_id;
ALTER TABLE discussion_ballots DROP COLUMN actor_id;
ALTER TABLE agent_sessions DROP COLUMN actor_id;
ALTER TABLE agent_api_keys DROP COLUMN actor_id;
ALTER TABLE agent_permissions DROP COLUMN actor_id;
ALTER TABLE mcp_sessions DROP COLUMN actor_id;
ALTER TABLE system_errors DROP COLUMN actor_id;
ALTER TABLE documents DROP COLUMN created_by_actor_id;
ALTER TABLE virtual_agent_usage DROP COLUMN actor_id;
ALTER TABLE error_log DROP COLUMN actor_id;
ALTER TABLE request_log DROP COLUMN actor_id;

-- Drop actor_id from agents and its constraints
ALTER TABLE agents DROP CONSTRAINT uq_agents_actor_id;
ALTER TABLE agents DROP CONSTRAINT fk_agents_actor;
ALTER TABLE agents DROP COLUMN actor_id;

-- Restore old indexes
CREATE INDEX idx_chat_messages_to_agent ON chat_messages (to_agent, id);
CREATE INDEX idx_chat_messages_unacked ON chat_messages (to_agent, channel) WHERE acked_at IS NULL;
CREATE INDEX idx_mail_to_agent_acked ON mail (to_agent, acked_at);
CREATE INDEX idx_discussion_participants_agent ON discussion_participants (agent, status);
CREATE INDEX idx_agent_sessions_agent ON agent_sessions (agent);
CREATE INDEX idx_agent_api_keys_agent ON agent_api_keys (agent);
CREATE INDEX idx_system_errors_agent ON system_errors (agent);
CREATE INDEX idx_mcp_sessions_agent ON mcp_sessions (agent);
CREATE INDEX idx_va_usage_agent_date ON virtual_agent_usage (agent, created_at);
CREATE INDEX idx_error_log_agent ON error_log (agent) WHERE agent IS NOT NULL;

-- Restore composite PKs with agent name
ALTER TABLE discussion_participants ADD PRIMARY KEY (discussion_id, agent);
ALTER TABLE discussion_ballots ADD PRIMARY KEY (vote_id, agent);
ALTER TABLE agent_permissions ADD PRIMARY KEY (agent, permission_id);

-- Restore FKs to agents(agent)
ALTER TABLE agent_sessions ADD CONSTRAINT agent_sessions_agent_fkey FOREIGN KEY (agent) REFERENCES agents(agent);
ALTER TABLE agent_api_keys ADD CONSTRAINT agent_api_keys_agent_fkey FOREIGN KEY (agent) REFERENCES agents(agent);
ALTER TABLE agent_permissions ADD CONSTRAINT agent_permissions_agent_fkey FOREIGN KEY (agent) REFERENCES agents(agent);
ALTER TABLE mcp_sessions ADD CONSTRAINT mcp_sessions_agent_fkey FOREIGN KEY (agent) REFERENCES agents(agent);
ALTER TABLE documents ADD CONSTRAINT documents_created_by_fkey FOREIGN KEY (created_by) REFERENCES agents(agent);
ALTER TABLE virtual_agent_usage ADD CONSTRAINT virtual_agent_usage_agent_fkey FOREIGN KEY (agent) REFERENCES agents(agent);

-- Restore agent_status view (original)
CREATE VIEW agent_status AS
SELECT agent,
       CASE
           WHEN virtual = TRUE THEN 'available'
           WHEN last_seen > NOW() - INTERVAL '15 minutes' THEN 'online'
           WHEN last_seen IS NOT NULL THEN 'offline'
           ELSE 'unknown'
       END AS status,
       last_seen,
       passphrase_rotated_at,
       registered_at,
       expertise,
       provider,
       model,
       virtual,
       personality,
       cost_budget_daily,
       cost_budget_monthly,
       CASE
           WHEN active_since IS NOT NULL AND active_since > NOW() - INTERVAL '30 minutes' THEN active_since
           ELSE NULL
       END AS active_since,
       cache_prompts,
       learning_enabled,
       max_tokens,
       temperature
FROM agents;

-- Drop actors table
DROP TABLE actors;
