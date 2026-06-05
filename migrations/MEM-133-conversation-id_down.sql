-- MEM-133 down: drop conversation_id columns (indexes drop with the columns).

ALTER TABLE chat_message_texts DROP COLUMN conversation_id;
ALTER TABLE virtual_agent_calls DROP COLUMN conversation_id;
