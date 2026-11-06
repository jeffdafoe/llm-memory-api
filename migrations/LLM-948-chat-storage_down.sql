-- Reverts LLM-948. The dropped tools_offered values are gone; the column
-- comes back empty.
BEGIN;

SET LOCAL lock_timeout = '5s';

DELETE FROM config WHERE key = 'sim_chat_retention_days';

ALTER TABLE virtual_agent_calls DROP COLUMN IF EXISTS tool_names;

ALTER TABLE chat_message_texts ADD COLUMN IF NOT EXISTS tools_offered jsonb;

COMMIT;
