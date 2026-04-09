-- MEM-110: Add hosted_realms config entry
--
-- Lists all realms hosted by this instance. Used for realm management UI,
-- invite code creation, and validation — instead of querying DISTINCT realms
-- from the actors table.

INSERT INTO config (key, value, description)
VALUES ('hosted_realms', 'llm-memory,zbbs',
        'Comma-separated list of all realms hosted by this instance. Used for realm management UI, invite code creation, and validation.');
