-- Sim-day logs are conversations (LLM-1032). A sim-shared virtual agent
-- writes each villager's day log at <villager>/conversations/<day>-sim-day,
-- and the slug-to-kind rule (dockind.Of) only matched a top-level
-- conversations/ folder, so those logs were saved as kind 'note'. The rule
-- now also matches one prefix segment before conversations/, unless that
-- segment is a reserved top-level folder (instructions, context, dreams,
-- tasks, learnings keep their own kind). Before LLM-1032 every slug this
-- WHERE selects was kind 'note'; the down migration relies on that.
--
-- Effects on the rows changed: enrichment skips them, search weighs them
-- with search_conversation_weight, and conversation retention applies. The
-- next cleanup run soft-deletes every log older than
-- conversation_retention_days and later empties it (tombstone).
--
-- updated_at is left alone, since search decay ages from it.
BEGIN;

UPDATE documents
   SET kind = 'conversation'
 WHERE slug ~ '^[^/]+/conversations/'
   AND split_part(slug, '/', 1) NOT IN ('conversations', 'instructions', 'context', 'dreams', 'tasks', 'learnings')
   AND kind <> 'conversation';

COMMIT;
