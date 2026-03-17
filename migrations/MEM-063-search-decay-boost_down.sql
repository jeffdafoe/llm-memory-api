-- MEM-063 rollback: Remove search decay & access boost.

DROP INDEX IF EXISTS documents_namespace_lower_slug_idx;
ALTER TABLE documents DROP COLUMN IF EXISTS kind;
ALTER TABLE documents DROP COLUMN IF EXISTS last_accessed;

DELETE FROM config WHERE key IN (
    'search_decay_halflife_task',
    'search_decay_halflife_learning',
    'search_decay_halflife_note',
    'search_decay_halflife_reference',
    'search_decay_halflife_instruction',
    'search_access_boost_max',
    'search_access_boost_window_days'
);
