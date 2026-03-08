-- MEM-042: Virtual agent rate limiting config
-- Adds config entries for rate limiting virtual agent API calls.

INSERT INTO config (key, value) VALUES ('virtual_agent_rate_limit', '10');
INSERT INTO config (key, value) VALUES ('virtual_agent_rate_window_seconds', '60');
INSERT INTO config (key, value) VALUES ('virtual_agent_cooldown_seconds', '300');
