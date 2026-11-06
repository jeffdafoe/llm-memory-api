-- Chat storage (LLM-948):
--  - chat_message_texts.tools_offered is dropped. It held the full tool
--    schemas on every row (most of the table's bytes) and nothing read it
--    back. DROP COLUMN does not free that space; a one-time
--    VACUUM FULL chat_message_texts after the deploy does.
--  - virtual_agent_calls.tool_names keeps the names of the tools offered on
--    a tool-use call, so a debugger can still see what a turn was offered.
--  - sim_chat_retention_days turns on the cleanup job's sim chat purge.
-- The ALTERs take ACCESS EXCLUSIVE locks; if they cannot get them within
-- the lock_timeout the migration fails whole, and the next deploy applies it
-- again.
BEGIN;

SET LOCAL lock_timeout = '5s';

ALTER TABLE chat_message_texts DROP COLUMN IF EXISTS tools_offered;

ALTER TABLE virtual_agent_calls ADD COLUMN IF NOT EXISTS tool_names text[];

COMMENT ON COLUMN virtual_agent_calls.tool_names IS 'Names of the tools offered on a tool-use call. NULL for plain-text calls and pre-LLM-948 rows.';

INSERT INTO config (key, value, description) VALUES
    ('sim_chat_retention_days', '14', 'Days to retain salem chat (texts sent by or delivered to salem-engine) before the cleanup job hard-deletes it. 0 = off.')
ON CONFLICT (key) DO NOTHING;

COMMIT;
