-- Reverts LLM-1032: nested <prefix>/conversations/ slugs go back to kind
-- 'note'. Top-level conversations/ rows were conversations before LLM-1032
-- and stay so. Rows that conversation retention already soft-deleted or
-- emptied stay deleted; this does not bring their content back. Revert the
-- code first, or the next save of such a slug sets the kind again.
BEGIN;

UPDATE documents
   SET kind = 'note'
 WHERE slug ~ '^[^/]+/conversations/'
   AND slug NOT LIKE 'conversations/%'
   AND slug NOT LIKE 'instructions/%'
   AND kind = 'conversation';

COMMIT;
