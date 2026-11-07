-- Chat storage (LLM-948), the destructive half: drop
-- chat_message_texts.tools_offered. It held the full tool schemas on every
-- tool-use row and nothing read it back. Since LLM-948's first migration
-- the code neither writes nor reads it (the offered tool names are kept in
-- virtual_agent_calls.tool_names), so no running API uses the column.
--
-- DROP COLUMN only changes the catalog; the old values stay on disk. Run
-- VACUUM FULL chat_message_texts once after this deploy, with the salem
-- engine stopped (it takes an ACCESS EXCLUSIVE lock for the whole
-- rewrite), to get the space back. VACUUM cannot run inside a transaction,
-- so it is not part of this migration.
--
-- If the DROP cannot get its lock within the lock_timeout, the migration
-- fails whole, and the next deploy applies it again.
BEGIN;

SET LOCAL lock_timeout = '5s';

ALTER TABLE chat_message_texts DROP COLUMN IF EXISTS tools_offered;

COMMIT;
