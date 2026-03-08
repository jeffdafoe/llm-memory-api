-- MEM-042 rollback
DELETE FROM config WHERE key IN ('virtual_agent_rate_limit', 'virtual_agent_rate_window_seconds', 'virtual_agent_cooldown_seconds');
