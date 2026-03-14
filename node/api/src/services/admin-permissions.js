// Admin UI permissions — enforces per-actor resource/action access on admin endpoints.
// Wildcard: resource='*', action='*' means full access (single row).
// Action hierarchy: read < write < delete.
//   - Checking 'read' passes if actor has read, write, or delete on that resource.
//   - Checking 'write' passes if actor has write or delete.
//   - Checking 'delete' requires delete specifically.

const pool = require('../db');

// Cache: actorId -> { permissions: [{resource, action}], expires }
const cache = new Map();
const CACHE_TTL_MS = 2 * 60 * 1000;

// Actions ranked by power — higher includes lower
const ACTION_RANK = { read: 1, write: 2, delete: 3 };

// Load all admin permissions for an actor from the database.
async function loadPermissions(actorId) {
    const result = await pool.query(
        'SELECT resource, action FROM admin_permissions WHERE actor_id = $1',
        [actorId]
    );
    return result.rows.map(r => ({ resource: r.resource, action: r.action }));
}

// Get permissions for an actor (cached).
async function getPermissions(actorId) {
    const cached = cache.get(actorId);
    if (cached && cached.expires > Date.now()) {
        return cached.permissions;
    }
    const permissions = await loadPermissions(actorId);
    cache.set(actorId, { permissions, expires: Date.now() + CACHE_TTL_MS });
    return permissions;
}

// Check if an actor has permission for a resource/action.
// Uses action hierarchy: having 'write' implies 'read', having 'delete' implies 'write' and 'read'.
async function hasPermission(actorId, resource, action) {
    const permissions = await getPermissions(actorId);

    // Check for global wildcard
    if (permissions.some(p => p.resource === '*' && p.action === '*')) {
        return true;
    }

    const requiredRank = ACTION_RANK[action];
    if (!requiredRank) return false;

    // Check grants on the specific resource
    for (const p of permissions) {
        if (p.resource !== resource && p.resource !== '*') continue;
        const grantedRank = ACTION_RANK[p.action];
        if (grantedRank && grantedRank >= requiredRank) {
            return true;
        }
        // Resource-level wildcard action
        if (p.action === '*') return true;
    }

    return false;
}

// Express middleware factory: require permission or return 403.
function requirePerm(resource, action) {
    return async (req, res, next) => {
        try {
            if (!Number.isInteger(req.actorId)) {
                return res.status(403).json({
                    error: { code: 'FORBIDDEN', message: 'Insufficient permissions' }
                });
            }
            const allowed = await hasPermission(req.actorId, resource, action);
            if (!allowed) {
                return res.status(403).json({
                    error: { code: 'FORBIDDEN', message: 'Insufficient permissions' }
                });
            }
            next();
        } catch (err) {
            console.error('Permission check error:', err.message);
            res.status(500).json({
                error: { code: 'INTERNAL', message: 'Permission check failed' }
            });
        }
    };
}

// Get permissions as a map for the login response: { resource: [action, ...], ... }
async function getPermissionMap(actorId) {
    const permissions = await getPermissions(actorId);
    const map = {};
    for (const p of permissions) {
        if (!map[p.resource]) map[p.resource] = [];
        map[p.resource].push(p.action);
    }
    return map;
}

// Clear cache for a specific actor (call after permission changes)
function clearCache(actorId) {
    if (actorId) {
        cache.delete(actorId);
    } else {
        cache.clear();
    }
}

module.exports = { hasPermission, requirePerm, getPermissions, getPermissionMap, clearCache };
