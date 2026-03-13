const pool = require('../db');
const { hash: hashToken } = require('../services/hashing');
const { resolveByName } = require('../services/actors');

// Cache session tokens in memory to avoid DB lookup on every request
// Key: bearer token, Value: { agent, actorId, expires }
const sessionCache = new Map();
const CACHE_TTL_MS = 5 * 60 * 1000; // 5 minutes

// Opportunistic heartbeat — update last_seen on every authenticated agent request
function heartbeat(actorId) {
    pool.query('UPDATE agents SET last_seen = NOW() WHERE actor_id = $1', [actorId]).catch(() => {});
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

    // Cache miss — check active agent sessions in database
    // After MEM-050: agent_sessions has actor_id, join with actors for name
    try {
        const result = await pool.query(
            `SELECT s.id, ac.name AS agent, s.actor_id, s.token_hash, s.token_salt, s.expires_at
             FROM agent_sessions s
             JOIN actors ac ON ac.id = s.actor_id
             WHERE s.expires_at > NOW()`
        );

        for (const row of result.rows) {
            const hash = hashToken(token, row.token_salt);
            if (hash === row.token_hash) {
                sessionCache.set(token, {
                    type: 'agent',
                    agent: row.agent,
                    actorId: row.actor_id,
                    expires: Math.min(
                        Date.now() + CACHE_TTL_MS,
                        new Date(row.expires_at).getTime()
                    )
                });
                req.authMethod = 'session';
                req.authenticatedAgent = row.agent;
                req.actorId = row.actor_id;
                heartbeat(row.actor_id);
                return next();
            }
        }
    } catch (err) {
        console.error('Auth agent session lookup error:', err.message);
    }

    // Check user sessions (admin UI)
    try {
        const result = await pool.query(
            "SELECT us.session_token, us.expires_at, u.id, u.username FROM user_sessions us JOIN users u ON u.id = us.user_id WHERE us.session_token = $1 AND us.expires_at > NOW()",
            [token]
        );

        if (result.rows.length > 0) {
            const row = result.rows[0];
            // Resolve user to actor for namespace permission checks
            const userActor = await resolveByName(row.username);
            const actorId = userActor ? userActor.id : null;
            sessionCache.set(token, {
                type: 'user',
                user: { id: row.id, username: row.username },
                actorId,
                expires: Math.min(
                    Date.now() + CACHE_TTL_MS,
                    new Date(row.expires_at).getTime()
                )
            });
            req.authMethod = 'session';
            req.authenticatedUser = { id: row.id, username: row.username };
            req.actorId = actorId;
            return next();
        }
    } catch (err) {
        console.error('Auth user session lookup error:', err.message);
    }

    return res.status(403).json({
        error: { code: 'FORBIDDEN', message: 'Invalid or expired session token' }
    });
}

// Exported so login/logout/rotate handlers can manage the cache
auth.sessionCache = sessionCache;

module.exports = auth;
