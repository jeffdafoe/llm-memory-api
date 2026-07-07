-- MEM-139 down: drop the sim_actor columns (the partial index drops with the column).

ALTER TABLE virtual_agent_calls DROP COLUMN sim_actor_id;
ALTER TABLE virtual_agent_calls DROP COLUMN sim_actor_name;
