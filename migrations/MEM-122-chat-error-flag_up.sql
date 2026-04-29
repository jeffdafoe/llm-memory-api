-- MEM-122: flag chat_message_texts rows that carry virtual-agent error
-- breadcrumbs ([Retrying], [Error]) so downstream forwarders can drop them.
--
-- The virtual-agent layer emits chat rows when a provider call fails or
-- a retry is being attempted ("[Retrying] Initial attempt failed: ...",
-- "[Error] <agent> is unavailable ..."). These rows are useful for admin
-- visibility but they should NOT be forwarded back into the agent's own
-- chat history when the next call is built — otherwise the agent reads
-- its own error breadcrumbs as in-context conversation and (observed
-- 2026-04-28 with salem-chronicler) treats them as legitimate input.
--
-- This adds an explicit flag. Writers set it on retry/error paths.
-- Readers (loadDirectChatHistory, loadChatHistory) filter it out.
-- Backward-compatible: existing rows default to FALSE, no behavior change
-- for non-error history.

BEGIN;

ALTER TABLE chat_message_texts
    ADD COLUMN is_error BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX idx_cmt_not_error
    ON chat_message_texts (sent_at)
    WHERE NOT is_error;

COMMIT;
