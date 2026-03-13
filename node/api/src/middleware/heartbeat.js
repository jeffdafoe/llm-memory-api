const pool = require('../db');

// Express middleware that piggybacks heartbeat updates onto normal API calls.
// Uses the authenticated agent identity (set by auth middleware) rather than
// request body fields, so admin routes that reference other agents by name
// don't accidentally heartbeat them.
function opportunisticHeartbeat(req, res, next) {
    const actorId = req.actorId || req.mcpActorId;
    if (actorId) {
        pool.query(
            'UPDATE agents SET last_seen = NOW() WHERE actor_id = $1',
            [actorId]
        ).catch(() => {}); // fire-and-forget — don't block the request
    }
    next();
}

module.exports = opportunisticHeartbeat;
