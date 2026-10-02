-- MEM-148 down — remove the salem-magistrate virtual agent (LLM-695).
--
-- Deletes the agent, its configuration and its access grant. Its chat history,
-- call log and bench book note go with the actor where they cascade; read them
-- first if they matter. The salem engine must have the court runner off (or be
-- rolled back to before LLM-695) or its daily sitting will fail to reach the VA.

BEGIN;

DELETE FROM virtual_agent_access
 WHERE virtual_agent_id IN (SELECT id FROM actors WHERE name = 'salem-magistrate');
DELETE FROM agent_configuration
 WHERE actor_id IN (SELECT id FROM actors WHERE name = 'salem-magistrate');
DELETE FROM actors WHERE name = 'salem-magistrate';

COMMIT;
