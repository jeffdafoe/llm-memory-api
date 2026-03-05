// Logs API request metadata to the request_log database table.
// Fire-and-forget inserts — errors are swallowed to avoid impacting request handling.
// Runs a daily cleanup to purge entries older than RETENTION_DAYS.

const pool = require('../db');

const RETENTION_DAYS = 7;

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

    // Capture request length from Content-Length header
    const requestLength = req.headers['content-length'] ? parseInt(req.headers['content-length'], 10) : null;

    // Hook res.end to capture status and duration after response completes
    const originalEnd = res.end;
    res.end = function (...args) {
        const duration = Date.now() - start;
        const status = res.statusCode;

        // Pull agent from whichever auth middleware ran
        let agent = null;
        if (req.authenticatedAgent) {
            agent = req.authenticatedAgent;
        } else if (req.mcpAgent) {
            agent = req.mcpAgent;
        } else if (req.authenticatedUser) {
            // Skip admin requests — they clutter the log with dashboard polling
            originalEnd.apply(res, args);
            return;
        }

        // Get client IP (respect X-Forwarded-For from nginx)
        const ip = req.headers['x-forwarded-for']?.split(',')[0]?.trim() || req.ip || null;

        // Capture response length from Content-Length header, or measure the body chunk
        let responseLength = res.getHeader('content-length') ? parseInt(res.getHeader('content-length'), 10) : null;
        if (responseLength === null && args[0]) {
            // args[0] is the body chunk passed to res.end()
            responseLength = Buffer.byteLength(args[0]);
        }

        // Fire-and-forget insert
        pool.query(
            `INSERT INTO request_log (method, path, status, duration_ms, agent, ip, request_length, response_length)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
            [req.method, displayPath, status, duration, agent, ip, requestLength, responseLength]
        ).catch(() => {});

        originalEnd.apply(res, args);
    };

    next();
}

// Query entries from DB. Supports since_id for incremental polling and limit for initial load.
// Aliases duration_ms → duration and timestamp → timestamp for frontend compatibility.
async function getEntries(sinceId, limit) {
    const cols = 'id, timestamp, method, path, status, duration_ms AS duration, agent, ip, request_length, response_length';
    if (sinceId) {
        const result = await pool.query(
            `SELECT ${cols} FROM request_log WHERE id > $1 ORDER BY id ASC LIMIT 500`,
            [sinceId]
        );
        return result.rows;
    }
    const result = await pool.query(
        `SELECT ${cols} FROM request_log ORDER BY id DESC LIMIT $1`,
        [limit || 100]
    );
    // Reverse so oldest-first (consistent with incremental polling order)
    return result.rows.reverse();
}

// Purge entries older than RETENTION_DAYS. Run periodically.
async function cleanup() {
    try {
        const result = await pool.query(
            `DELETE FROM request_log WHERE timestamp < NOW() - INTERVAL '1 day' * $1`,
            [RETENTION_DAYS]
        );
        if (result.rowCount > 0) {
            console.log(`request-log cleanup: purged ${result.rowCount} entries older than ${RETENTION_DAYS} days`);
        }
    } catch (err) {
        console.error('request-log cleanup error:', err.message);
    }
}

// Run cleanup once on startup and then every 24 hours
cleanup();
setInterval(cleanup, 24 * 60 * 60 * 1000);

module.exports = { requestLog, getEntries };
