-- MEM-037: Virtual agent support
-- Adds columns to agents table for API-backed virtual agents.
-- Virtual agents auto-respond via provider APIs when triggered.

ALTER TABLE agents ADD COLUMN virtual BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE agents ADD COLUMN personality TEXT;
ALTER TABLE agents ADD COLUMN api_key VARCHAR(512);
ALTER TABLE agents ADD COLUMN configuration TEXT;
ALTER TABLE agents ADD COLUMN cost TEXT;

-- Update agent_status view to include virtual flag and personality
CREATE OR REPLACE VIEW agent_status AS
SELECT agent,
       CASE
           WHEN virtual = TRUE THEN 'online'
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
       cost
FROM agents;

-- Encryption key for API keys (AES-256-GCM)
INSERT INTO config (key, value) VALUES ('virtual_agent_encryption_key', '');
