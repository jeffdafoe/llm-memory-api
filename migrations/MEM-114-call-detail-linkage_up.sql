-- MEM-114: Link virtual_agent_calls back to virtual_agent_usage
-- and add failure tracking to virtual_agent_usage.

-- Reference from call detail back to the usage row (no FK — calls get purged independently)
ALTER TABLE virtual_agent_calls ADD COLUMN usage_id BIGINT;

-- Add status column to usage table so failures are recorded alongside successes
ALTER TABLE virtual_agent_usage ADD COLUMN status TEXT NOT NULL DEFAULT 'success';
ALTER TABLE virtual_agent_usage ADD COLUMN error_message TEXT;
