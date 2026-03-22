-- MEM-071: Add global_bootstrap config key
-- Prepended to all agent startup instructions (virtual and non-virtual)

INSERT INTO config (key, value, description) VALUES ('global_bootstrap', '', 'Prepended to all agent startup instructions (virtual and non-virtual)')
ON CONFLICT (key) DO NOTHING;
