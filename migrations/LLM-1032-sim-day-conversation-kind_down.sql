-- Reverts LLM-1032: <prefix>/conversations/ slugs go back to kind 'note'.
-- The WHERE is the up migration's: reserved top-level folders were never
-- changed, and every other slug it selects was kind 'note' before LLM-1032
-- (rows written while it was live included). Top-level conversations/ rows
-- were conversations before and are not selected. Rows that conversation
-- retention already soft-deleted or emptied stay deleted; this does not
-- bring their content back. Revert the code first, or the next save of such
-- a slug sets the kind again.
BEGIN;

UPDATE documents
   SET kind = 'note'
 WHERE slug ~ '^[^/]+/conversations/'
   AND split_part(slug, '/', 1) NOT IN ('conversations', 'instructions', 'context', 'dreams', 'tasks', 'learnings')
   AND kind = 'conversation';

COMMIT;
