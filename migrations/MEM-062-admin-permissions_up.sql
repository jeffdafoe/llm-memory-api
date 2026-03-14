-- MEM-062: Admin UI permissions — granular resource/action grants per actor
CREATE TABLE admin_permissions (
    id          SERIAL PRIMARY KEY,
    actor_id    INTEGER NOT NULL REFERENCES actors(id) ON DELETE CASCADE,
    resource    VARCHAR(50) NOT NULL,
    action      VARCHAR(50) NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(actor_id, resource, action)
);

-- Seed: jeff (id=11) gets wildcard access
INSERT INTO admin_permissions (actor_id, resource, action) VALUES (11, '*', '*');

-- Seed: wendy (id=9) gets scoped access
INSERT INTO admin_permissions (actor_id, resource, action) VALUES (9, 'dashboard', 'read');
INSERT INTO admin_permissions (actor_id, resource, action) VALUES (9, 'notes', 'write');
INSERT INTO admin_permissions (actor_id, resource, action) VALUES (9, 'comms', 'read');
