-- Chat storage (LLM-948), the additive half:
--  - virtual_agent_calls.tool_names keeps the names of the tools offered on
--    a tool-use call, so a debugger can still see what a turn was offered
--    once chat_message_texts.tools_offered is gone.
--  - sim_chat_retention_days turns on the cleanup job's sim chat purge.
-- The code no longer writes or reads chat_message_texts.tools_offered. The
-- column is dropped by a later migration (LLM-948b), deployed after this one,
-- so no running API ever writes to a column that is gone.
-- The ALTER takes an ACCESS EXCLUSIVE lock; if it cannot get it within the
-- lock_timeout the migration fails whole, and the next deploy applies it
-- again.
BEGIN;

SET LOCAL lock_timeout = '5s';

ALTER TABLE virtual_agent_calls ADD COLUMN IF NOT EXISTS tool_names text[];

COMMENT ON COLUMN virtual_agent_calls.tool_names IS 'Names of the tools offered on a tool-use call. NULL for plain-text calls and pre-LLM-948 rows.';

INSERT INTO config (key, value, description) VALUES
    ('sim_chat_retention_days', '14', 'Days to retain salem chat (texts sent by or delivered to salem-engine) before the cleanup job hard-deletes it. 0 = off.')
ON CONFLICT (key) DO NOTHING;

COMMIT;
