const crypto = require('crypto');
const pool = require('../db');

function hashToken(plaintext, salt) {
    return crypto.pbkdf2Sync(plaintext, salt, 100000, 64, 'sha512').toString('hex');
}

// Cache agent tokens in memory to avoid DB lookup on every request
// Key: bearer token, Value: { agent, expires }
const tokenCache = new Map();
const CACHE_TTL_MS = 5 * 60 * 1000; // 5 minutes

async function auth(req, res, next) {
    const header = req.headers.authorization;

    if (!header) {
        return res.status(401).json({
            error: { code: 'UNAUTHORIZED', message: 'Missing Authorization header' }
        });
    }

    const token = header.replace('Bearer ', '');

    // Shared API key fallback (grandfathered)
    if (token === process.env.MEMORY_API_KEY) {
        req.authMethod = 'api_key';
        return next();
    }

    // Check token cache first
    const cached = tokenCache.get(token);
    if (cached && cached.expires > Date.now()) {
        req.authMethod = 'agent_token';
        req.authenticatedAgent = cached.agent;
        return next();
    }

    // Look up all active agents with tokens
    try {
        const result = await pool.query(
            "SELECT agent, token_hash, token_salt FROM agents WHERE status = 'active' AND token_hash IS NOT NULL"
        );

        for (const row of result.rows) {
            const hash = hashToken(token, row.token_salt);
            if (hash === row.token_hash) {
                // Cache the successful match
                tokenCache.set(token, {
                    agent: row.agent,
                    expires: Date.now() + CACHE_TTL_MS
                });
                req.authMethod = 'agent_token';
                req.authenticatedAgent = row.agent;
                return next();
            }
        }
    } catch (err) {
        console.error('Auth token lookup error:', err.message);
    }

    return res.status(403).json({
        error: { code: 'FORBIDDEN', message: 'Invalid API key or agent token' }
    });
}

module.exports = auth;
