-- MEM-056: Namespace permissions
-- Adds per-actor read/write/delete access control on namespaces.
-- Enforced on all document and memory routes.

-- Step 1: Create namespace_permissions table
CREATE TABLE namespace_permissions (
    id SERIAL PRIMARY KEY,
    actor_id INTEGER NOT NULL REFERENCES actors(id) ON DELETE CASCADE,
    namespace VARCHAR(100) NOT NULL,
    can_read BOOLEAN NOT NULL DEFAULT FALSE,
    can_write BOOLEAN NOT NULL DEFAULT FALSE,
    can_delete BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (actor_id, namespace)
);

CREATE INDEX idx_ns_perm_actor ON namespace_permissions (actor_id);
CREATE INDEX idx_ns_perm_namespace ON namespace_permissions (namespace);

-- Step 2: Ensure admin users have actor records.
-- MEM-050 only seeded actors from the agents table.
-- Users need actors too so they can have namespace permissions.
INSERT INTO actors (name, type)
SELECT u.username, 'user'
FROM users u
WHERE NOT EXISTS (SELECT 1 FROM actors a WHERE a.name = u.username)
ON CONFLICT (name) DO NOTHING;

-- Also link users to their actor records (add actor_id column to users if not present)
-- We don't alter the users table here — the auth middleware will resolve user -> actor by name.

-- Step 3: Seed permissions for existing actors.
-- Wildcard '/' means access to all namespaces (present and future).

-- Jeff (admin user) — full wildcard access
INSERT INTO namespace_permissions (actor_id, namespace, can_read, can_write, can_delete)
SELECT id, '/', TRUE, TRUE, TRUE FROM actors WHERE name = 'jeff';

-- work agent — own namespace + shared + home
INSERT INTO namespace_permissions (actor_id, namespace, can_read, can_write, can_delete)
SELECT id, 'work', TRUE, TRUE, TRUE FROM actors WHERE name = 'work'
UNION ALL
SELECT id, 'shared', TRUE, TRUE, TRUE FROM actors WHERE name = 'work'
UNION ALL
SELECT id, 'home', TRUE, TRUE, TRUE FROM actors WHERE name = 'work';

-- home agent — own namespace + shared + work
INSERT INTO namespace_permissions (actor_id, namespace, can_read, can_write, can_delete)
SELECT id, 'home', TRUE, TRUE, TRUE FROM actors WHERE name = 'home'
UNION ALL
SELECT id, 'shared', TRUE, TRUE, TRUE FROM actors WHERE name = 'home'
UNION ALL
SELECT id, 'work', TRUE, TRUE, TRUE FROM actors WHERE name = 'home';

-- wendy — own namespace only
INSERT INTO namespace_permissions (actor_id, namespace, can_read, can_write, can_delete)
SELECT id, 'wendy', TRUE, TRUE, TRUE FROM actors WHERE name = 'wendy';

-- search-general (virtual agent) — own namespace only
INSERT INTO namespace_permissions (actor_id, namespace, can_read, can_write, can_delete)
SELECT id, 'search-general', TRUE, TRUE, TRUE FROM actors WHERE name = 'search-general';

-- system — own namespace only
INSERT INTO namespace_permissions (actor_id, namespace, can_read, can_write, can_delete)
SELECT id, 'system', TRUE, TRUE, TRUE FROM actors WHERE name = 'system';
