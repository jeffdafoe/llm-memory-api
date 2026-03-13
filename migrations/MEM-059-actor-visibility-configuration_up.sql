-- MEM-059: Actor visibility configuration
-- Controls which additional actors a logged-in admin UI user can see.
-- Implicit: every actor can always see themselves.
-- Wildcard: target_actor_id IS NULL means the viewer can see ALL actors.

CREATE TABLE actor_visibility_configuration (
    id SERIAL PRIMARY KEY,
    actor_id INTEGER NOT NULL REFERENCES actors(id) ON DELETE CASCADE,
    target_actor_id INTEGER REFERENCES actors(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (actor_id, target_actor_id)
);

-- Partial unique index for wildcard rows (only one NULL target per actor)
CREATE UNIQUE INDEX idx_avc_wildcard ON actor_visibility_configuration (actor_id) WHERE target_actor_id IS NULL;

CREATE INDEX idx_avc_actor ON actor_visibility_configuration (actor_id);
CREATE INDEX idx_avc_target ON actor_visibility_configuration (target_actor_id);

-- Seed: jeff sees everything (wildcard)
INSERT INTO actor_visibility_configuration (actor_id, target_actor_id)
SELECT id, NULL FROM actors WHERE name = 'jeff';

-- Seed: wendy sees herself (implicit, no rows needed)
-- Add explicit grants if she should see specific agents later.
