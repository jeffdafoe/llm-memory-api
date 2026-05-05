-- MEM-127 down: drop scene_structure column from chat_message_texts.

ALTER TABLE chat_message_texts DROP COLUMN scene_structure;
