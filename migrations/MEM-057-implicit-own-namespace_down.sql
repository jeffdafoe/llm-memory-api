-- MEM-057 rollback: Re-insert self-referential permission rows removed by the up migration.
-- Only re-seeds the ones that MEM-056 originally created.

INSERT INTO namespace_permissions (actor_id, namespace, can_read, can_write, can_delete)
SELECT id, 'wendy', TRUE, TRUE, TRUE FROM actors WHERE name = 'wendy'
ON CONFLICT (actor_id, namespace) DO NOTHING;

INSERT INTO namespace_permissions (actor_id, namespace, can_read, can_write, can_delete)
SELECT id, 'search-general', TRUE, TRUE, TRUE FROM actors WHERE name = 'search-general'
ON CONFLICT (actor_id, namespace) DO NOTHING;

INSERT INTO namespace_permissions (actor_id, namespace, can_read, can_write, can_delete)
SELECT id, 'system', TRUE, TRUE, TRUE FROM actors WHERE name = 'system'
ON CONFLICT (actor_id, namespace) DO NOTHING;

-- work and home also had self-referential rows
INSERT INTO namespace_permissions (actor_id, namespace, can_read, can_write, can_delete)
SELECT id, 'work', TRUE, TRUE, TRUE FROM actors WHERE name = 'work'
ON CONFLICT (actor_id, namespace) DO NOTHING;

INSERT INTO namespace_permissions (actor_id, namespace, can_read, can_write, can_delete)
SELECT id, 'home', TRUE, TRUE, TRUE FROM actors WHERE name = 'home'
ON CONFLICT (actor_id, namespace) DO NOTHING;
