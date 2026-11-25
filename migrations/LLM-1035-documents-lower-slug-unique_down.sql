-- Back to the exact-case unique index on live notes. Every state the
-- lowercased index allows also passes the exact-case one, so this cannot
-- fail on data.
BEGIN;

SET LOCAL lock_timeout = '5s';

CREATE UNIQUE INDEX documents_namespace_slug_active_key
    ON documents (namespace, slug)
    WHERE deleted_at IS NULL;

DROP INDEX documents_namespace_lower_slug_active_key;

COMMIT;
