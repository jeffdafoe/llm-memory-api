-- GDPR retention (LLM-1024): config keys for the cleanup job's two new purges.
--  - access_request_retention_days and pending_access_request_retention_days
--    turn on the purge of signup records (access requests, unused invites) of
--    people who never registered.
--  - error_log_retention_days turns on the purge of error_log and
--    system_errors.
-- ON CONFLICT keeps this a no-op on a fresh install, where seed.sql already
-- inserted the keys.
BEGIN;

INSERT INTO config (key, value, description) VALUES
    ('access_request_retention_days', '30', 'Days the cleanup job keeps the signup records of people who never registered: an invite that expired unused, with its access request (counted from the invite''s expiry), and an approved or rejected access request that never had an invite (counted from its review). 0 = off.'),
    ('pending_access_request_retention_days', '90', 'Days the cleanup job keeps an access request whose status is still pending, counted from when it was made. Status alone decides: a pending row is deleted even if reviewed_at is set. 0 = off.'),
    ('error_log_retention_days', '90', 'Days the cleanup job keeps error_log and system_errors rows before hard-deleting them. 0 = off.')
ON CONFLICT (key) DO NOTHING;

COMMIT;
