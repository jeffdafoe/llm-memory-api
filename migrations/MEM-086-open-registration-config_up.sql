-- MEM-086: Add config option for open registration (no invite code required)
-- When set to 'true', the registration endpoint skips invite code validation.
-- Default is empty string (disabled). Set to 'true' to enable open registration.

INSERT INTO config (key, value, description)
VALUES ('open_registration', '', 'When set to "true", new users can register without an invite code')
ON CONFLICT (key) DO NOTHING;
