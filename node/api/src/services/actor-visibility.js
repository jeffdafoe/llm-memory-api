// Actor visibility — controls which actors a logged-in admin UI user can see.
// Implicit: every actor can always see themselves.
// Wildcard: a row with target_actor_id IS NULL means the viewer sees all actors.

const pool = require('../db');

// Cache: actorId -> { visibleIds: Set<number> | null, expires }
// null visibleIds means wildcard (see everything)
const cache = new Map();
const CACHE_TTL_MS = 2 * 60 * 1000;

function isValidActorId(id) {
    return Number.isInteger(id) && id > 0;
}

// Load visibility grants for an actor from the database.
// Returns null (wildcard — see everything) or a Set of visible actor IDs.
async function loadVisibility(actorId) {
    const result = await pool.query(
        'SELECT target_actor_id FROM actor_visibility_configuration WHERE actor_id = $1',
        [actorId]
    );

    // Check for wildcard (NULL target)
    for (const row of result.rows) {
        if (row.target_actor_id === null) {
            return null; // wildcard — no filtering needed
        }
    }

    // Build set of explicitly visible actor IDs (plus self, always implicit)
    const ids = new Set();
    ids.add(actorId); // always see yourself
    for (const row of result.rows) {
        ids.add(row.target_actor_id);
    }
    return ids;
}

// Get visible actor IDs for an actor (cached).
// Returns null (see everything) or Set<number>.
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
    if (visibleIds === null) {
        return true; // wildcard
    }
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
