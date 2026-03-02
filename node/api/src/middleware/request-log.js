// In-memory ring buffer that captures API request metadata for the admin dashboard.
// No database involvement — data is ephemeral and resets on restart.

const MAX_ENTRIES = 1000;
const buffer = [];
let nextId = 1;

function requestLog(req, res, next) {
    const start = Date.now();
    const entry = {
        id: nextId++,
        method: req.method,
        path: req.originalUrl || req.url,
        agent: null,
        timestamp: new Date().toISOString(),
        status: null,
        duration: null
    };

    // Capture agent after auth middleware has run.
    // We hook res.end so we pick up the final state.
    const originalEnd = res.end;
    res.end = function (...args) {
        entry.status = res.statusCode;
        entry.duration = Date.now() - start;

        // Pull agent from auth middleware (set on req by auth.js)
        if (req.authenticatedAgent) {
            entry.agent = req.authenticatedAgent;
        } else if (req.authenticatedUser) {
            entry.agent = 'admin:' + req.authenticatedUser.username;
        }

        buffer.push(entry);

        // Trim if over capacity
        if (buffer.length > MAX_ENTRIES) {
            buffer.splice(0, buffer.length - MAX_ENTRIES);
        }

        originalEnd.apply(res, args);
    };

    next();
}

// Return entries newer than the given id (for incremental polling).
// If sinceId is 0 or omitted, returns the last `limit` entries.
function getEntries(sinceId, limit) {
    if (sinceId) {
        return buffer.filter(e => e.id > sinceId);
    }
    const start = Math.max(0, buffer.length - (limit || 100));
    return buffer.slice(start);
}

module.exports = { requestLog, getEntries };
