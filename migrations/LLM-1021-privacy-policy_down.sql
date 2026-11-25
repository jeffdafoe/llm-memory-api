-- Reverts LLM-1021's config keys. Without them /privacy shows the "no policy
-- published" page and both scripts fall back to 7 days.
BEGIN;

DELETE FROM config WHERE key IN (
    'privacy_policy',
    'request_log_retention_days',
    'backup_retention_days'
);

COMMIT;
