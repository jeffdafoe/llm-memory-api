-- MEM-142 down — un-enroll salem-vendor and drop the roster. Run this before
-- MEM-141 down (it clears the 'sim-shared' value the restored enum can't hold).

BEGIN;

UPDATE agent_configuration
   SET dream_mode = 'none'
 WHERE actor_id IN (SELECT id FROM actors WHERE name = 'salem-vendor');

DROP TABLE IF EXISTS sim_shared_actor;

COMMIT;
