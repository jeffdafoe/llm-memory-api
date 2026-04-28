-- MEM-121 down: drop scene_id columns and indexes.

DROP INDEX IF EXISTS idx_cmt_scene;
ALTER TABLE chat_message_texts DROP COLUMN IF EXISTS scene_id;

DROP INDEX IF EXISTS idx_va_calls_scene;
ALTER TABLE virtual_agent_calls DROP COLUMN IF EXISTS scene_id;
