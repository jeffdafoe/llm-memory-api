-- Reverts LLM-948's additive migration.
BEGIN;

SET LOCAL lock_timeout = '5s';

DELETE FROM config WHERE key = 'sim_chat_retention_days';

ALTER TABLE virtual_agent_calls DROP COLUMN IF EXISTS tool_names;

COMMIT;
