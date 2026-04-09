// Actor visibility — controls which actors a logged-in admin UI user can see.
// Two layers of filtering:
//   1. Realm scoping — actors only see others that share at least one realm.
//   2. Explicit grants — within a realm, visibility can be further restricted
//      via actor_visibility_configuration (wildcard or per-actor grants).
// Implicit: every actor can always see themselves.

const pool = require('../db');

// Cache: actorId -> { visibleIds: Set<number> | null, expires }
// null visibleIds means no filtering needed (see everything in realm)
const cache = new Map();
const CACHE_TTL_MS = 2 * 60 * 1000;

function isValidActorId(id) {
    return Number.isInteger(id) && id > 0;
}

// Load the set of actor IDs sharing at least one realm with the viewer.
// Returns a Set of actor IDs (always includes self).
async function loadRealmPeers(actorId) {
    // Find all actors whose realms array overlaps with the viewer's realms.
    // The && operator checks array overlap in PostgreSQL.
    const result = await pool.query(
        `SELECT a2.id FROM actors a1
         JOIN actors a2 ON a1.realms && a2.realms
         WHERE a1.id = $1`,
        [actorId]
    );
    const ids = new Set();
    ids.add(actorId);
    for (const row of result.rows) {
        ids.add(row.id);
    }
    return ids;
}

// Load visibility grants for an actor from the database.
// Returns null (wildcard — see everything in realm) or a Set of visible actor IDs.
async function loadVisibility(actorId) {
    // First get realm peers — this is the outer boundary
    const realmPeers = await loadRealmPeers(actorId);

    // Then check explicit visibility grants
    const result = await pool.query(
        'SELECT target_actor_id FROM actor_visibility_configuration WHERE actor_id = $1',
        [actorId]
    );

    // Check for wildcard (NULL target) — sees all agents within their realm(s)
    for (const row of result.rows) {
        if (row.target_actor_id === null) {
            return realmPeers;
        }
    }

    // No explicit grants and no wildcard — default to seeing all realm peers.
    // The explicit grant system is for restricting within a realm, not for
    // granting access. If you have no grants, you see your whole realm.
    if (result.rows.length === 0) {
        return realmPeers;
    }

    // Explicit grants exist — intersect with realm peers
    const ids = new Set();
    ids.add(actorId);
    for (const row of result.rows) {
        if (realmPeers.has(row.target_actor_id)) {
            ids.add(row.target_actor_id);
        }
    }
    return ids;
}

// Get visible actor IDs for an actor (cached).
// Returns Set<number> of visible actor IDs.
async function getVisibleActorIds(actorId) {
    if (!isValidActorId(actorId)) {
        return new Set(); // fail closed — see nothing
    }
    const cached = cache.get(actorId);
    if (cached && cached.expires > Date.now()) {
        return cached.visibleIds;
    }

    const visibleIds = await loadVisibility(actorId);
    cache.set(actorId, { visibleIds, expires: Date.now() + CACHE_TTL_MS });
    return visibleIds;
}

// Check if a viewer can see a specific target actor.
async function canSee(viewerActorId, targetActorId) {
    if (!isValidActorId(viewerActorId) || !isValidActorId(targetActorId)) {
        return false; // fail closed
    }
    // Always see yourself
    if (viewerActorId === targetActorId) {
        return true;
    }

    const visibleIds = await getVisibleActorIds(viewerActorId);
    return visibleIds.has(targetActorId);
}

// Clear cache for a specific actor (call after visibility changes)
function clearCache(actorId) {
    if (actorId) {
        cache.delete(actorId);
    } else {
        cache.clear();
    }
}

module.exports = { getVisibleActorIds, canSee, clearCache };
