-- Add request timeout config for virtual agent provider API calls.
-- Value is in seconds. Prevents hung requests from blocking retries indefinitely.
INSERT INTO config (key, value) VALUES ('virtual_agent_request_timeout', '120')
ON CONFLICT (key) DO NOTHING;
