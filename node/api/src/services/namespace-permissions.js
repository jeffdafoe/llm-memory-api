// Namespace permission checks — enforces per-actor read/write/delete access on namespaces.
// Used by both document routes (REST) and MCP tool handlers.
// Wildcard '/' in a permission row means access to all namespaces.

const pool = require('../db');

// Cache: actorId -> { permissions: Map<namespace, {read,write,delete}>, expires }
const cache = new Map();
const CACHE_TTL_MS = 2 * 60 * 1000; // 2 minutes (shorter than actor cache — perms change more often)

// Load all namespace permissions for an actor from the database.
// Returns a Map of namespace -> { read, write, delete }.
async function loadPermissions(actorId) {
    const result = await pool.query(
        'SELECT namespace, can_read, can_write, can_delete FROM namespace_permissions WHERE actor_id = $1',
        [actorId]
    );

    const perms = new Map();
    for (const row of result.rows) {
        perms.set(row.namespace, {
            read: row.can_read,
            write: row.can_write,
            delete: row.can_delete
        });
    }
    return perms;
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

// Check if an actor has the specified access on a namespace.
// operation: 'read' | 'write' | 'delete'
// Returns true/false.
async function hasAccess(actorId, actorName, actorType, namespace, operation) {
    // Implicit: agents always have full access to their own namespace.
    // Only applies to agents — users don't have matching namespaces.
    if (actorType === 'agent' && actorName && namespace === actorName) {
        return true;
    }

    const perms = await getPermissions(actorId);

    // Check wildcard first — '/' grants access to everything
    const wildcard = perms.get('/');
    if (wildcard && wildcard[operation]) {
        return true;
    }

    // Check specific namespace grant
    const specific = perms.get(namespace);
    if (specific && specific[operation]) {
        return true;
    }

    return false;
}

// Require access — throws 403 if denied. Use in route handlers.
async function requireAccess(actorId, actorName, actorType, namespace, operation) {
    const allowed = await hasAccess(actorId, actorName, actorType, namespace, operation);
    if (!allowed) {
        throw Object.assign(
            new Error(`Actor "${actorName}" does not have ${operation} access to namespace "${namespace}"`),
            { statusCode: 403 }
        );
    }
}

// Get all namespaces an actor can read — used to filter namespace:* queries.
// Returns an array of namespace strings, or null if the actor has wildcard access
// (null means "no filtering needed").
async function getReadableNamespaces(actorId, actorName, actorType) {
    const perms = await getPermissions(actorId);

    // Wildcard means all namespaces — no filtering needed
    const wildcard = perms.get('/');
    if (wildcard && wildcard.read) {
        return null;
    }

    const namespaces = [];
    for (const [ns, perm] of perms) {
        if (ns !== '/' && perm.read) {
            namespaces.push(ns);
        }
    }

    // Implicit: agents' own namespace is always readable
    if (actorType === 'agent' && actorName && !namespaces.includes(actorName)) {
        namespaces.push(actorName);
    }

    return namespaces;
}

// Clear cache for a specific actor (call after permission changes)
function clearCache(actorId) {
    if (actorId) {
        cache.delete(actorId);
    } else {
        cache.clear();
    }
}

// Reserved namespace sentinels — cannot be used as actual content namespaces.
// '/' is the wildcard permission grant, '*' is the wildcard search qualifier.
const RESERVED_NAMESPACES = ['/', '*'];

function validateNamespace(namespace) {
    if (RESERVED_NAMESPACES.includes(namespace)) {
        throw Object.assign(
            new Error(`"${namespace}" is a reserved namespace and cannot be used for content`),
            { statusCode: 400 }
        );
    }
}

module.exports = { hasAccess, requireAccess, getReadableNamespaces, clearCache, validateNamespace };
