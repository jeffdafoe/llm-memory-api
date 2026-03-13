-- MEM-057: Implicit own-namespace permissions
-- Own-namespace access is now handled in code (hasAccess / getReadableNamespaces
-- return true when actorName === namespace). Remove the redundant self-referential
-- permission rows seeded by MEM-056.

-- Remove self-referential grants (where namespace = actor name).
-- Keeps cross-namespace grants (work↔home, work↔shared, etc.) and wildcard '/'.
DELETE FROM namespace_permissions
WHERE id IN (
    SELECT np.id
    FROM namespace_permissions np
    JOIN actors a ON a.id = np.actor_id
    WHERE np.namespace = a.name
);
