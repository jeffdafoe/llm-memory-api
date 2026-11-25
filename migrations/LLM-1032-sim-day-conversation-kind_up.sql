-- Sim-day logs are conversations (LLM-1032). A sim-shared virtual agent
-- writes each villager's day log at <villager>/conversations/<day>-sim-day,
-- and the slug-to-kind rule (dockind.Of) only matched a top-level
-- conversations/ folder, so those logs were saved as kind 'note'. The rule
-- now also matches one prefix segment before conversations/; this brings the
-- existing rows into line with it.
--
-- Effects on the rows changed: enrichment skips them, search weighs them
-- with search_conversation_weight, and conversation retention applies. The
-- next cleanup run soft-deletes every log older than
-- conversation_retention_days and later empties it (tombstone).
--
-- instructions/ is checked before conversations in the rule, so an
-- instructions/conversations/... slug stays an instruction. updated_at is
-- left alone, since search decay ages from it.
BEGIN;

UPDATE documents
   SET kind = 'conversation'
 WHERE slug ~ '^([^/]+/)?conversations/'
   AND slug NOT LIKE 'instructions/%'
   AND kind <> 'conversation';

COMMIT;
