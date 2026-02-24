-- MEM-015: System agent for server-generated notifications
-- Registers a "system" agent that the server uses to send automated messages.
-- No passphrase — this agent is used internally by the server, never authenticates.

INSERT INTO agents (agent, status)
VALUES ('system', 'active')
ON CONFLICT (agent) DO NOTHING;
