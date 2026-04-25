const pool = require('../db');
const { verify } = require('../services/hashing');
const { logError } = require('../services/logger');
const { SESSION_KIND } = require('../constants');
const { validateSessionToken } = require('../services/sessions');

// Cache session tokens in memory to avoid DB lookup on every request
// Key: bearer token, Value: { agent, actorId, expires }
const sessionCache = new Map();
const CACHE_TTL_MS = 5 * 60 * 1000; // 5 minutes

// API key auth — accepts a 64-char hex token issued via the admin
// `agent_api_keys` table. Iterates over non-revoked keys and verifies the hash.
// Returns { agent, actorId } on match, null otherwise. Updates last_used_at on
// success (fire-and-forget). Mirrors the path used by mcp-auth.js so a single
// API key works against both /mcp and /v1.
async function tryApiKeyAuth(token) {
    if (typeof token !== 'string' || !/^[0-9a-f]{64}$/i.test(token)) {
        return null;
    }
    const keys = await pool.query(
        `SELECT ac.name AS agent, ak.actor_id, ak.key_hash, ak.key_salt
         FROM agent_api_keys ak
         JOIN actors ac ON ac.id = ak.actor_id
         WHERE ak.revoked_at IS NULL`
    );
    for (const row of keys.rows) {
        if (verify(token, row.key_salt, row.key_hash)) {
            pool.query(
                'UPDATE agent_api_keys SET last_used_at = NOW() WHERE actor_id = $1 AND key_salt = $2',
                [row.actor_id, row.key_salt]
            ).catch(() => {});
            return { agent: row.agent, actorId: row.actor_id };
        }
    }
    return null;
}

// Opportunistic heartbeat — update last_seen on every authenticated agent request
function heartbeat(actorId) {
    pool.query('UPDATE actors SET last_seen = NOW() WHERE id = $1', [actorId]).catch(() => {});
}

// Routes that don't require authentication
const UNAUTHENTICATED_ROUTES = [
    '/agent/login',
    '/admin/login',
];

async function auth(req, res, next) {
    const route = req.path;

    if (UNAUTHENTICATED_ROUTES.includes(route)) {
        return next();
    }

    const header = req.headers.authorization;

    if (!header) {
        return res.status(401).json({
            error: { code: 'UNAUTHORIZED', message: 'Missing Authorization header' }
        });
    }

    const token = header.replace('Bearer ', '');

    // Everything else requires a valid session token
    const cached = sessionCache.get(token);
    if (cached && cached.expires > Date.now()) {
        req.authMethod = 'session';
        if (cached.type === 'user') {
            req.authenticatedUser = cached.user;
            req.actorId = cached.actorId;
        } else {
            req.authenticatedAgent = cached.agent;
            req.actorId = cached.actorId;
            heartbeat(cached.actorId);
        }
        return next();
    }

    // Cache miss — check active API sessions (agents) in unified sessions table
    try {
        const apiRow = await validateSessionToken(token, SESSION_KIND.API);
        if (apiRow) {
            sessionCache.set(token, {
                type: 'agent',
                agent: apiRow.name,
                actorId: apiRow.actor_id,
                expires: Math.min(
                    Date.now() + CACHE_TTL_MS,
                    new Date(apiRow.expires_at).getTime()
                )
            });
            req.authMethod = 'session';
            req.authenticatedAgent = apiRow.name;
            req.actorId = apiRow.actor_id;
            heartbeat(apiRow.actor_id);
            return next();
        }
    } catch (err) {
        console.error('Auth API session lookup error:', err.message);
    }

    // Check web sessions (admin UI) in unified sessions table
    try {
        const webRow = await validateSessionToken(token, SESSION_KIND.WEB);
        if (webRow) {
            sessionCache.set(token, {
                type: 'user',
                user: { id: webRow.actor_id, username: webRow.name },
                actorId: webRow.actor_id,
                expires: Math.min(
                    Date.now() + CACHE_TTL_MS,
                    new Date(webRow.expires_at).getTime()
                )
            });
            req.authMethod = 'session';
            req.authenticatedUser = { id: webRow.actor_id, username: webRow.name };
            req.actorId = webRow.actor_id;
            return next();
        }
    } catch (err) {
        console.error('Auth web session lookup error:', err.message);
    }

    // Try API key as a service-to-service auth path. Lets engines
    // (e.g. salem) authenticate as an agent without managing 24-hour session
    // refresh cycles. Cached on hit so the PBKDF2 verify isn't redone every
    // call. Same `agent_api_keys` table that mcp-auth.js uses.
    try {
        const result = await tryApiKeyAuth(token);
        if (result) {
            sessionCache.set(token, {
                type: 'agent',
                agent: result.agent,
                actorId: result.actorId,
                expires: Date.now() + CACHE_TTL_MS
            });
            req.authMethod = 'api-key';
            req.authenticatedAgent = result.agent;
            req.actorId = result.actorId;
            heartbeat(result.actorId);
            return next();
        }
    } catch (err) {
        console.error('Auth API key lookup error:', err.message);
    }

    return res.status(403).json({
        error: { code: 'FORBIDDEN', message: 'Invalid or expired session token' }
    });
}

// Exported so login/logout/rotate handlers can manage the cache
auth.sessionCache = sessionCache;

module.exports = auth;
