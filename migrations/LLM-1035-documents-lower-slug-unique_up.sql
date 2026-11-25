-- One live note per (namespace, LOWER(slug)). MEM-040 made every note
-- lookup ignore case ("Slugs are preserved as-is in the DB - only
-- comparisons are lowered") but kept the exact-case unique index, so a
-- plain save, a restore or a folder move could still make "notes/a" and
-- "Notes/A" live together. Read, edit and delete then matched both rows.
--
-- The new index serves the existing LOWER(slug) = LOWER($n) lookups on live
-- notes. documents_namespace_lower_slug_idx stays: restore and the search
-- join also read soft-deleted rows. The exact-case index is dropped; the new
-- one is stricter.
--
-- If a live case-variant pair exists, CREATE UNIQUE INDEX fails, the
-- migration fails whole, and the deploy stops. Merge the pair by hand and
-- deploy again. (None existed on 2026-11-25.)
BEGIN;

SET LOCAL lock_timeout = '5s';

CREATE UNIQUE INDEX documents_namespace_lower_slug_active_key
    ON documents (namespace, LOWER(slug))
    WHERE deleted_at IS NULL;

DROP INDEX documents_namespace_slug_active_key;

COMMIT;
