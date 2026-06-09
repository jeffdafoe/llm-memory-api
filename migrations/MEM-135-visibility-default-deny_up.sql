-- MEM-135: visibility default flips to self-only (default-deny).
-- The code change makes an actor with zero actor_visibility_configuration
-- rows see only itself (previously: all realm peers). Trusted agents that
-- relied on the open default get explicit wildcard rows here so they keep
-- their realm-wide view. Superadmins (*:*) bypass visibility in code and
-- need no row, but the rows are harmless belt-and-braces for them.
-- Idempotent: ON CONFLICT DO NOTHING (jeff was already seeded by MEM-059;
-- the partial unique index idx_avc_wildcard catches duplicate wildcards).
--
-- MUST run BEFORE the MEM-135 code deploys, or work/home go roster-blind
-- until it does.

INSERT INTO actor_visibility_configuration (actor_id, target_actor_id)
SELECT id, NULL FROM actors WHERE name IN ('jeff', 'wendy', 'work', 'home')
ON CONFLICT DO NOTHING;
