-- MEM-144 down — drop virtual_agent_calls.served_by (LLM-560).
--
-- Rollback contract: safe to run ahead of or behind a code rollback. The
-- LLM-560 code writes served_by as an ordinary INSERT column, so rolling the
-- column back while the new code is still deployed breaks logCall's insert —
-- run this only together with, or after, reverting the code. The reverse order
-- (code back, column left in place) is harmless: the column simply stops being
-- written and reads NULL from then on.
--
-- Dropping this discards the recorded upstream attribution for every call
-- logged since the migration. That data is not reconstructible — OpenRouter
-- does not report it retroactively — so the accounting question that motivated
-- LLM-560 becomes unanswerable again for the affected window.

ALTER TABLE virtual_agent_calls DROP COLUMN IF EXISTS served_by;
