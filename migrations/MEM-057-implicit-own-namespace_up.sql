-- MEM-057: Implicit own-namespace permissions
-- Own-namespace access is now handled in code (hasAccess / getReadableNamespaces
-- return true when actorType === 'agent' and actorName === namespace).
-- Remove the redundant self-referential permission rows seeded by MEM-056.

-- Remove agent self-referential grants (where namespace = actor name).
-- Only targets agents — user permissions remain explicit.
-- Keeps cross-namespace grants (work↔home, work↔shared, etc.) and wildcard '/'.
DELETE FROM namespace_permissions np
USING actors a
WHERE a.id = np.actor_id
  AND a.type = 'agent'
  AND np.namespace = a.name;
