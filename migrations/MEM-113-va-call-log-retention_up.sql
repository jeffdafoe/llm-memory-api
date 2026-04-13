-- MEM-113: VA call log retention config
INSERT INTO config (key, value, description)
VALUES ('va_call_log_retention_days', '5', 'Days to retain virtual_agent_calls rows before hard-deleting')
ON CONFLICT (key) DO NOTHING;
