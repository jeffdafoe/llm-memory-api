const pool = require('../db');

// Express middleware that piggybacks heartbeat updates onto normal API calls.
// Any request with an agent identifier in the body gets a fire-and-forget
// last_seen update, so active agents stay "online" without explicit heartbeats.
function opportunisticHeartbeat(req, res, next) {
    const agent = req.body.from_agent || req.body.agent;
    if (agent) {
        pool.query(
            'UPDATE agents SET last_seen = NOW() WHERE agent = $1',
            [agent]
        ).catch(() => {}); // fire-and-forget — don't block the request
    }
    next();
}

module.exports = opportunisticHeartbeat;
