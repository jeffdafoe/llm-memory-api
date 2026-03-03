-- MEM-028: Increase agent online threshold from 5 to 15 minutes.
-- Agents often have gaps between tool calls longer than 5 minutes,
-- causing false "offline" status during active sessions.

CREATE OR REPLACE VIEW agent_status AS
SELECT agent,
       CASE
           WHEN last_seen > NOW() - INTERVAL '15 minutes' THEN 'online'
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
