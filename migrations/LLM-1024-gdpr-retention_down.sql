-- Reverts LLM-1024's config keys. With them gone the two purges fail their
-- config read and log an error each run, so revert the code first.
BEGIN;

DELETE FROM config WHERE key IN (
    'access_request_retention_days',
    'pending_access_request_retention_days',
    'error_log_retention_days'
);

COMMIT;
