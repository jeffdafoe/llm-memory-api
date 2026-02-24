const crypto = require('crypto');
const pool = require('../db');

function hashToken(plaintext, salt) {
    return crypto.pbkdf2Sync(plaintext, salt, 100000, 64, 'sha512').toString('hex');
}

// Cache session tokens in memory to avoid DB lookup on every request
// Key: bearer token, Value: { agent, expires }
const sessionCache = new Map();
const CACHE_TTL_MS = 5 * 60 * 1000; // 5 minutes

// Routes that don't require authentication
const UNAUTHENTICATED_ROUTES = [
    '/agent/register',
    '/agent/register/ack',
    '/agent/login',
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
        req.authenticatedAgent = cached.agent;
        return next();
    }

    // Cache miss — check active sessions in database
    try {
        const result = await pool.query(
            "SELECT id, agent, token_hash, token_salt, expires_at FROM agent_sessions WHERE expires_at > NOW()"
        );

        for (const row of result.rows) {
            const hash = hashToken(token, row.token_salt);
            if (hash === row.token_hash) {
                sessionCache.set(token, {
                    agent: row.agent,
                    expires: Math.min(
                        Date.now() + CACHE_TTL_MS,
                        new Date(row.expires_at).getTime()
                    )
                });
                req.authMethod = 'session';
                req.authenticatedAgent = row.agent;
                return next();
            }
        }
    } catch (err) {
        console.error('Auth session lookup error:', err.message);
    }

    return res.status(403).json({
        error: { code: 'FORBIDDEN', message: 'Invalid or expired session token' }
    });
}

// Exported so login/logout/rotate handlers can manage the cache
auth.sessionCache = sessionCache;
auth.hashToken = hashToken;

module.exports = auth;
