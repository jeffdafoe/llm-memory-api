-- MEM-069: Expand actors.status constraint for virtual agent lifecycle
--
-- The original constraint only allowed 'active', but virtual agents need
-- 'available', 'degraded', and 'error' statuses for their lifecycle.
-- The agent_status view reads actors.status for virtual agents, so these
-- values must be allowed.

ALTER TABLE actors DROP CONSTRAINT chk_actors_status;
ALTER TABLE actors ADD CONSTRAINT chk_actors_status
    CHECK (status IN ('active', 'available', 'degraded', 'error'));
