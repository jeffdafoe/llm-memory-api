-- MEM-108: Remove virtual_agent_max_retries config
-- Retry count is now derived from the virtual_agent_retry_backoff cadence array.

DELETE FROM config WHERE key = 'virtual_agent_max_retries';
