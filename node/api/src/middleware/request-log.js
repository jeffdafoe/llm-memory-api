// Logs API request metadata to the request_log database table.
// Fire-and-forget inserts — errors are swallowed to avoid impacting request handling.
// Retention cleanup is handled by the db-cleanup cron job.

const pool = require('../db');

// Paths to exclude from logging (polling endpoints, static assets, health checks)
const EXCLUDED_PATHS = ['/v1/admin/api-log', '/v1/admin/error-log', '/v1/admin/dashboard', '/v1/admin/notes/reindex-status', '/v1/admin/notes/reindex-clear', '/admin/', '/health'];

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

    // Capture request length from Content-Length header
    const requestLength = req.headers['content-length'] ? parseInt(req.headers['content-length'], 10) : null;

    // Track bytes written via res.write() (SSE/streaming responses)
    let bytesWritten = 0;
    const originalWrite = res.write;
    res.write = function (...args) {
        if (args[0]) {
            bytesWritten += Buffer.byteLength(args[0]);
        }
        return originalWrite.apply(res, args);
    };

    // Hook res.end to capture status and duration after response completes
    const originalEnd = res.end;
    res.end = function (...args) {
        const duration = Date.now() - start;
        const status = res.statusCode;

        // Pull actor_id from whichever auth middleware ran
        let actorId = null;
        if (req.actorId) {
            actorId = req.actorId;
        } else if (req.mcpActorId) {
            actorId = req.mcpActorId;
        } else if (req.authenticatedUser) {
            // Skip admin requests — they clutter the log with dashboard polling
            originalEnd.apply(res, args);
            return;
        }

        // Get client IP (respect X-Forwarded-For from nginx)
        const ip = req.headers['x-forwarded-for']?.split(',')[0]?.trim() || req.ip || null;

        // Capture response length: Content-Length header, or body in end(), or accumulated write() bytes
        let responseLength = res.getHeader('content-length') ? parseInt(res.getHeader('content-length'), 10) : null;
        if (responseLength === null && args[0]) {
            responseLength = bytesWritten + Buffer.byteLength(args[0]);
        } else if (responseLength === null && bytesWritten > 0) {
            responseLength = bytesWritten;
        }

        // Fire-and-forget insert
        pool.query(
            `INSERT INTO request_log (method, path, status, duration_ms, actor_id, ip, request_length, response_length)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
            [req.method, displayPath, status, duration, actorId, ip, requestLength, responseLength]
        ).catch(() => {});

        originalEnd.apply(res, args);
    };

    next();
}

// Query entries from DB. Supports since_id for incremental polling and limit for initial load.
// Aliases duration_ms → duration and timestamp → timestamp for frontend compatibility.
// visibleActorIds: null (no filtering) or Set/Array of actor IDs to restrict results.
async function getEntries(sinceId, limit, visibleActorIds) {
    const cols = `r.id, r.timestamp, r.method, r.path, r.status,
                  r.duration_ms AS duration, ac.name AS agent, r.ip,
                  r.request_length, r.response_length`;
    // Build optional visibility filter
    let visFilter = '';
    const params = [];
    if (visibleActorIds) {
        const ids = Array.from(visibleActorIds);
        if (ids.length === 0) {
            return []; // can't see anyone — return nothing
        }
        // Filter to requests from visible actors (exclude rows with no actor — those are unauthenticated)
        const placeholders = ids.map((id, i) => '$' + (i + 1));
        visFilter = ` AND r.actor_id IN (${placeholders.join(', ')})`;
        params.push(...ids);
    }
    if (sinceId) {
        params.push(sinceId);
        const result = await pool.query(
            `SELECT ${cols} FROM request_log r LEFT JOIN actors ac ON ac.id = r.actor_id WHERE r.id > $${params.length}${visFilter} ORDER BY r.id ASC LIMIT 500`,
            params
        );
        return result.rows;
    }
    params.push(limit || 100);
    const result = await pool.query(
        `SELECT ${cols} FROM request_log r LEFT JOIN actors ac ON ac.id = r.actor_id WHERE 1=1${visFilter} ORDER BY r.id DESC LIMIT $${params.length}`,
        params
    );
    // Reverse so oldest-first (consistent with incremental polling order)
    return result.rows.reverse();
}

module.exports = { requestLog, getEntries };
