-- MEM-114 rollback
ALTER TABLE virtual_agent_calls DROP COLUMN IF EXISTS usage_id;
ALTER TABLE virtual_agent_usage DROP COLUMN IF EXISTS error_message;
ALTER TABLE virtual_agent_usage DROP COLUMN IF EXISTS status;
