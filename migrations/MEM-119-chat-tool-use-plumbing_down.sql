-- MEM-119 down: remove tool-use columns and revert chat history hours.

ALTER TABLE chat_message_texts DROP COLUMN tool_calls;
ALTER TABLE chat_message_texts DROP COLUMN tool_call_id;
ALTER TABLE chat_message_texts DROP COLUMN tools_offered;

UPDATE config SET value = '4' WHERE key = 'virtual_agent_chat_history_hours';
