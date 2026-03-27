// Actor resolution service — maps between agent names and integer actor IDs.
// All external APIs continue to use agent names; this service handles the
// internal translation to/from actors.id for database queries.
//
// Actor identity is capability-based (no type column):
//   - Has agent_configuration row → can authenticate as agent
//   - Has password_hash → can authenticate as web user
//   - Has both → dual identity (e.g. Wendy)

const pool = require('../db');

// In-memory cache with TTL. Actor records rarely change, so caching is safe.
const nameCache = new Map(); // name -> { actor, expires }
const idCache = new Map();   // id -> { actor, expires }
const CACHE_TTL_MS = 5 * 60 * 1000;

async function resolveByName(name) {
    const cached = nameCache.get(name);
    if (cached && cached.expires > Date.now()) return cached.actor;

    const result = await pool.query('SELECT id, name FROM actors WHERE name = $1', [name]);
    if (result.rows.length === 0) return null;

    const actor = result.rows[0];
    cacheActor(actor);
    return actor;
}

async function resolveById(id) {
    const cached = idCache.get(id);
    if (cached && cached.expires > Date.now()) return cached.actor;

    const result = await pool.query('SELECT id, name FROM actors WHERE id = $1', [id]);
    if (result.rows.length === 0) return null;

    const actor = result.rows[0];
    cacheActor(actor);
    return actor;
}

// Resolve multiple names in a single query. Returns Map<name, actor>.
async function resolveMultipleByName(names) {
    const result = new Map();
    const uncached = [];

    for (const name of names) {
        const cached = nameCache.get(name);
        if (cached && cached.expires > Date.now()) {
            result.set(name, cached.actor);
        } else {
            uncached.push(name);
        }
    }

    if (uncached.length > 0) {
        const rows = await pool.query('SELECT id, name FROM actors WHERE name = ANY($1)', [uncached]);
        for (const row of rows.rows) {
            cacheActor(row);
            result.set(row.name, row);
        }
    }

    return result;
}

// Require an actor to exist by name. Throws 404 if not found.
async function requireByName(name) {
    const actor = await resolveByName(name);
    if (!actor) {
        throw Object.assign(new Error(`Agent "${name}" is not registered`), { statusCode: 404 });
    }
    return actor;
}

function cacheActor(actor) {
    const entry = { actor, expires: Date.now() + CACHE_TTL_MS };
    nameCache.set(actor.name, entry);
    idCache.set(actor.id, entry);
}

function clearCache() {
    nameCache.clear();
    idCache.clear();
}

// Check if an actor can access a virtual agent.
// Access is granted if any of:
//   1. The actor is the creator (owner) of the virtual agent
//   2. The actor has an admin_permissions row (admin)
//   3. There's a virtual_agent_access row with grantee_actor_id = NULL (public)
//   4. There's a virtual_agent_access row with grantee_actor_id = actor's id
async function canAccessVirtualAgent(actorId, virtualAgentId) {
    const result = await pool.query(`
        SELECT EXISTS(
            -- Creator/owner check
            SELECT 1 FROM actors WHERE id = $2 AND created_by = $1
            UNION ALL
            -- Admin check
            SELECT 1 FROM admin_permissions WHERE actor_id = $1
            UNION ALL
            -- ACL check (public or explicit grant)
            SELECT 1 FROM virtual_agent_access
            WHERE virtual_agent_id = $2
              AND (grantee_actor_id IS NULL OR grantee_actor_id = $1)
        ) AS has_access
    `, [actorId, virtualAgentId]);
    return result.rows[0].has_access;
}

module.exports = { resolveByName, resolveById, resolveMultipleByName, requireByName, clearCache, canAccessVirtualAgent };
