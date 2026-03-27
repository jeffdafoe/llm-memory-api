// Note-level sharing permissions — allows actors to share individual notes or folders
// with specific actors or all actors. Complements namespace_permissions which controls
// broad namespace access.

const pool = require('../db');

// Create a share. slug_pattern is either an exact slug or a folder prefix ending in '/'.
// granteeActorId is null for share-with-all.
async function createShare({ ownerNamespace, slugPattern, granteeActorId, canRead, canWrite, canDelete, grantedBy }) {
    // Validate: share-with-all cannot grant delete
    if (granteeActorId === null && canDelete) {
        throw Object.assign(
            new Error('Share-with-all cannot grant delete permission'),
            { statusCode: 400 }
        );
    }

    // Check for existing active share (same owner, slug, grantee)
    const existing = await pool.query(
        `SELECT id FROM note_permissions
         WHERE owner_namespace = $1 AND slug_pattern = $2
           AND ${granteeActorId === null ? 'grantee_actor_id IS NULL' : 'grantee_actor_id = $4'}
           AND revoked_at IS NULL`,
        granteeActorId === null
            ? [ownerNamespace, slugPattern]
            : [ownerNamespace, slugPattern, null, granteeActorId]
    );

    if (existing.rows.length > 0) {
        // Update existing share permissions
        const id = existing.rows[0].id;
        const result = await pool.query(
            `UPDATE note_permissions SET can_read = $1, can_write = $2, can_delete = $3
             WHERE id = $4 RETURNING *`,
            [canRead, canWrite, canDelete, id]
        );
        return result.rows[0];
    }

    const result = await pool.query(
        `INSERT INTO note_permissions (owner_namespace, slug_pattern, grantee_actor_id, can_read, can_write, can_delete, granted_by)
         VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *`,
        [ownerNamespace, slugPattern, granteeActorId, canRead, canWrite, canDelete, grantedBy]
    );
    return result.rows[0];
}

// Revoke a share by ID. Soft-revoke via revoked_at timestamp.
async function revokeShare(shareId) {
    const result = await pool.query(
        `UPDATE note_permissions SET revoked_at = NOW() WHERE id = $1 AND revoked_at IS NULL RETURNING *`,
        [shareId]
    );
    if (result.rows.length === 0) {
        throw Object.assign(
            new Error('Share not found or already revoked'),
            { statusCode: 404 }
        );
    }
    return result.rows[0];
}

// List shares for a given owner namespace (what have I shared?).
// Joins actor names for display.
async function listSharesByOwner(ownerNamespace) {
    const result = await pool.query(
        `SELECT np.*, a.name AS grantee_name, g.name AS granted_by_name
         FROM note_permissions np
         LEFT JOIN actors a ON np.grantee_actor_id = a.id
         JOIN actors g ON np.granted_by = g.id
         WHERE np.owner_namespace = $1 AND np.revoked_at IS NULL
         ORDER BY np.slug_pattern, np.grantee_actor_id`,
        [ownerNamespace]
    );
    return result.rows;
}

// List shares granted to a specific actor (what's shared with me?).
// Includes both specific grants and share-with-all grants.
async function listSharesForRecipient(actorId) {
    const result = await pool.query(
        `SELECT np.*, g.name AS granted_by_name
         FROM note_permissions np
         JOIN actors g ON np.granted_by = g.id
         WHERE (np.grantee_actor_id = $1 OR np.grantee_actor_id IS NULL)
           AND np.revoked_at IS NULL
           AND np.can_read = true
         ORDER BY np.owner_namespace, np.slug_pattern`,
        [actorId]
    );
    return result.rows;
}

// List all active shares (admin view).
async function listAllShares() {
    const result = await pool.query(
        `SELECT np.*, o.name AS owner_name, a.name AS grantee_name, g.name AS granted_by_name
         FROM note_permissions np
         JOIN actors o ON o.name = np.owner_namespace
         LEFT JOIN actors a ON np.grantee_actor_id = a.id
         JOIN actors g ON np.granted_by = g.id
         WHERE np.revoked_at IS NULL
         ORDER BY np.owner_namespace, np.slug_pattern, np.grantee_actor_id`
    );
    return result.rows;
}

// Get shares for a specific note slug. Checks both exact matches and folder prefixes.
// Returns array of applicable permission rows.
async function getSharesForSlug(ownerNamespace, slug, actorId) {
    const result = await pool.query(
        `SELECT * FROM note_permissions
         WHERE owner_namespace = $1
           AND (slug_pattern = $2 OR $2 LIKE slug_pattern || '%')
           AND (grantee_actor_id = $3 OR grantee_actor_id IS NULL)
           AND revoked_at IS NULL
           AND can_read = true`,
        [ownerNamespace, slug, actorId]
    );
    return result.rows;
}

// Check if an actor has a specific permission on a note.
// operation: 'read' | 'write' | 'delete'
async function hasNoteAccess(ownerNamespace, slug, actorId, operation) {
    const column = 'can_' + operation;
    const result = await pool.query(
        `SELECT 1 FROM note_permissions
         WHERE owner_namespace = $1
           AND (slug_pattern = $2 OR $2 LIKE slug_pattern || '%')
           AND (grantee_actor_id = $3 OR grantee_actor_id IS NULL)
           AND revoked_at IS NULL
           AND ${column} = true
         LIMIT 1`,
        [ownerNamespace, slug, actorId]
    );
    return result.rows.length > 0;
}

// Get all namespaces + slug patterns shared with an actor (for search integration).
// Returns rows with owner_namespace and slug_pattern.
async function getSharedReadAccess(actorId) {
    const result = await pool.query(
        `SELECT DISTINCT owner_namespace, slug_pattern
         FROM note_permissions
         WHERE (grantee_actor_id = $1 OR grantee_actor_id IS NULL)
           AND revoked_at IS NULL
           AND can_read = true`,
        [actorId]
    );
    return result.rows;
}

// List actual documents shared with an actor, grouped by owner namespace.
// Returns documents with their share permissions.
async function listSharedDocuments(actorId) {
    const result = await pool.query(
        `SELECT DISTINCT d.namespace, d.slug, d.title, d.updated_at,
                np.can_read, np.can_write, np.can_delete
         FROM note_permissions np
         JOIN documents d ON d.namespace = np.owner_namespace
           AND (np.slug_pattern = d.slug OR (np.slug_pattern LIKE '%/' AND d.slug LIKE np.slug_pattern || '%'))
         WHERE (np.grantee_actor_id = $1 OR np.grantee_actor_id IS NULL)
           AND np.revoked_at IS NULL AND np.can_read = true
           AND d.deleted_at IS NULL
         ORDER BY d.namespace, d.slug`,
        [actorId]
    );
    return result.rows;
}

module.exports = {
    createShare,
    revokeShare,
    listSharesByOwner,
    listSharesForRecipient,
    listAllShares,
    getSharesForSlug,
    hasNoteAccess,
    getSharedReadAccess,
    listSharedDocuments,
};
