-- MEM-119: Tool-use plumbing on direct chat (Salem M6.4 — engine ↔ NPC over chat).
--
-- Three nullable columns on chat_message_texts:
--   tool_calls    — assistant's emitted tool calls (array of {id, name, input}).
--                   Set on the VA reply row when the model called tools.
--   tool_call_id  — links a tool-result message back to the assistant's tool_call.
--                   Set on engine-sent messages that are tool results (e.g.
--                   look_around resolved against engine state).
--   tools_offered — the tool spec the sender is offering for this turn. Set on
--                   engine-sent perception/result messages so handleDirectChat
--                   can branch into the tool-use path. Per-message rather than
--                   per-thread because chat_messages has no thread/session
--                   table to attach a config to.
--
-- All NULL for existing rows; existing chat callers (no tools) ignore them.
--
-- Also bumps virtual_agent_chat_history_hours from 4 to 72. Salem NPCs need
-- multi-game-day continuity over chat — the prior default (4 hours) cut off
-- yesterday's interactions. The sliding window still bounds context cost.

ALTER TABLE chat_message_texts ADD COLUMN tool_calls JSONB;
ALTER TABLE chat_message_texts ADD COLUMN tool_call_id TEXT;
ALTER TABLE chat_message_texts ADD COLUMN tools_offered JSONB;

UPDATE config SET value = '72' WHERE key = 'virtual_agent_chat_history_hours';
