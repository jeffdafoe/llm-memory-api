// In-memory ring buffer that captures API request metadata for the admin dashboard.
// No database involvement — data is ephemeral and resets on restart.

const MAX_ENTRIES = 1000;
const buffer = [];
let nextId = 1;

// Paths to exclude from logging (polling endpoints, static assets, health checks)
const EXCLUDED_PATHS = ['/v1/admin/api-log', '/v1/admin/dashboard', '/admin/', '/health'];

function requestLog(req, res, next) {
    const path = req.originalUrl || req.url;
    if (EXCLUDED_PATHS.some(p => path.startsWith(p))) {
        return next();
    }

    const start = Date.now();

    // For MCP requests, extract the tool/method from the JSON-RPC body
    let displayPath = path;
    if (path === '/mcp' && req.body) {
        const rpcMethod = req.body.method;
        if (rpcMethod === 'tools/call' && req.body.params?.name) {
            displayPath = '/mcp → ' + req.body.params.name;
        } else if (rpcMethod) {
            displayPath = '/mcp → ' + rpcMethod;
        }
    }

    const entry = {
        id: nextId++,
        method: req.method,
        path: displayPath,
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

        // Pull agent from whichever auth middleware ran
        if (req.authenticatedAgent) {
            entry.agent = req.authenticatedAgent;
        } else if (req.mcpAgent) {
            entry.agent = req.mcpAgent;
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
// If sinceId >= nextId, the server restarted — return all entries so the client resyncs.
function getEntries(sinceId, limit) {
    if (sinceId) {
        if (sinceId >= nextId) {
            // Client has a stale id from before a restart — return everything
            return buffer.slice();
        }
        return buffer.filter(e => e.id > sinceId);
    }
    const start = Math.max(0, buffer.length - (limit || 100));
    return buffer.slice(start);
}

module.exports = { requestLog, getEntries };
