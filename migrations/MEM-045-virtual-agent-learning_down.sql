-- MEM-045 rollback
DELETE FROM config WHERE key IN ('virtual_agent_learning_enabled', 'virtual_agent_learning_min_tokens');
