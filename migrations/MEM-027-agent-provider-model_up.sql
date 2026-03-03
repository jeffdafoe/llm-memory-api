-- MEM-027: Add provider and model columns to agents table.
-- Tracks which AI provider (anthropic, openai, etc.) and model each agent uses.

ALTER TABLE agents ADD COLUMN provider VARCHAR(50);
ALTER TABLE agents ADD COLUMN model VARCHAR(100);

-- Update the agent_status view to include the new fields
CREATE OR REPLACE VIEW agent_status AS
SELECT agent,
       CASE
           WHEN last_seen > NOW() - INTERVAL '5 minutes' THEN 'online'
           WHEN last_seen IS NOT NULL THEN 'offline'
           ELSE 'unknown'
       END AS status,
       last_seen,
       passphrase_rotated_at,
       registered_at,
       expertise,
       provider,
       model
FROM agents;
