-- Privacy policy page (LLM-1021): config keys.
--  - privacy_policy holds the operator's policy in markdown; GET /privacy
--    serves it. Empty shows a "no policy published" page.
--  - request_log_retention_days and backup_retention_days replace the 7
--    days hard-coded in db-cleanup.sh and pg-backup.sh, so the policy can
--    state them with a placeholder; the page and the scripts apply the same
--    validation and the same fallback of 7.
-- ON CONFLICT keeps this a no-op on a fresh install, where seed.sql already
-- inserted the keys.
BEGIN;

INSERT INTO config (key, value, description) VALUES
    ('privacy_policy', '', 'The privacy policy, in markdown, served at /privacy. Empty shows a page saying no policy is published. {{name}} placeholders are filled only for keys ending in _retention_days. See docs/privacy-policy-example.md.'),
    ('request_log_retention_days', '7', 'Days db-cleanup.sh keeps request_log rows (method, path, client IP) before deleting them. A whole number from 1 to 999999; anything else, or a failed read, uses 7. The privacy policy can show it as {{request_log_retention_days}}.'),
    ('backup_retention_days', '7', 'Days pg-backup.sh keeps the nightly dumps of every database on this server before pruning them. A whole number from 1 to 999999; anything else, or a failed read, uses 7. The privacy policy can show it as {{backup_retention_days}}.')
ON CONFLICT (key) DO NOTHING;

COMMIT;
