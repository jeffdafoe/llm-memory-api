-- Add created_by to actors (ownership)
ALTER TABLE actors ADD COLUMN created_by INTEGER REFERENCES actors(id);

-- Set existing actors created_by to jeff (id=11)
UPDATE actors SET created_by = 11;

-- Virtual agent access control
CREATE TABLE virtual_agent_access (
    id SERIAL PRIMARY KEY,
    virtual_agent_id INTEGER NOT NULL REFERENCES actors(id) ON DELETE CASCADE,
    grantee_actor_id INTEGER REFERENCES actors(id) ON DELETE CASCADE,  -- NULL = public access
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Unique constraint: one row per virtual_agent + grantee (including NULL)
CREATE UNIQUE INDEX uq_vaa_agent_grantee
    ON virtual_agent_access (virtual_agent_id, grantee_actor_id)
    WHERE grantee_actor_id IS NOT NULL;
CREATE UNIQUE INDEX uq_vaa_agent_public
    ON virtual_agent_access (virtual_agent_id)
    WHERE grantee_actor_id IS NULL;

-- Grant public access to all existing virtual agents (preserve current behavior)
INSERT INTO virtual_agent_access (virtual_agent_id, grantee_actor_id)
SELECT ac.actor_id, NULL
FROM agent_configuration ac
WHERE ac.virtual = true;
