-- MEM-135 rollback: remove the trusted-agent wildcard visibility rows.
-- jeff's wildcard row predates this migration (seeded by MEM-059) and is
-- deliberately left in place.

DELETE FROM actor_visibility_configuration
 WHERE target_actor_id IS NULL
   AND actor_id IN (SELECT id FROM actors WHERE name IN ('wendy', 'work', 'home'));
