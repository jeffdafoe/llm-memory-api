// Validates bearer tokens on the /mcp endpoint.
// Accepts two token types:
//   1. JWT tokens issued by /oauth/token (for claude.com OAuth flow)
//   2. API keys from agent_api_keys table (for Claude Code direct auth)
// Sets req.mcpAgent and req.mcpPermissions for downstream handlers.

const jwt = require('jsonwebtoken');
const pool = require('../db');
const { hash } = require('../services/hashing');

const JWT_SECRET = process.env.JWT_SECRET;

function getResourceMetadataUrl(req) {
    if (process.env.BASE_URL) {
        return `${process.env.BASE_URL}/.well-known/oauth-protected-resource`;
    }
    return `${req.protocol}://${req.get('host')}/.well-known/oauth-protected-resource`;
}

// Try to authenticate with an API key from agent_api_keys.
// Returns { agent, permissions } on success, null on failure.
async function tryApiKeyAuth(token) {
    const keys = await pool.query(
        'SELECT ak.agent, ak.key_hash, ak.key_salt FROM agent_api_keys ak WHERE ak.revoked_at IS NULL'
    );

    for (const row of keys.rows) {
        const computed = hash(token, row.key_salt);
        if (computed === row.key_hash) {
            // Update last_used_at
            pool.query(
                'UPDATE agent_api_keys SET last_used_at = NOW() WHERE agent = $1 AND key_salt = $2',
                [row.agent, row.key_salt]
            ).catch(() => {});

            // Fetch permissions
            const perms = await pool.query(
                'SELECT p.name FROM agent_permissions ap JOIN permissions p ON p.id = ap.permission_id WHERE ap.agent = $1',
                [row.agent]
            );

            return {
                agent: row.agent,
                permissions: perms.rows.map(r => r.name)
            };
        }
    }

    return null;
}

async function mcpAuth(req, res, next) {
    const header = req.headers.authorization;

    if (!header || !header.startsWith('Bearer ')) {
        res.set('WWW-Authenticate', `Bearer resource_metadata="${getResourceMetadataUrl(req)}"`);
        return res.status(401).json({
            error: 'unauthorized',
            error_description: 'Missing or invalid Authorization header'
        });
    }

    const token = header.slice(7);

    // Try JWT first (fast path)
    try {
        const decoded = jwt.verify(token, JWT_SECRET);
        req.mcpAgent = decoded.agent;
        req.mcpPermissions = decoded.permissions || [];
        return next();
    } catch (err) {
        // Not a valid JWT — fall through to API key check
    }

    // Try API key auth (slower path — iterates keys)
    try {
        const result = await tryApiKeyAuth(token);
        if (result) {
            req.mcpAgent = result.agent;
            req.mcpPermissions = result.permissions;
            return next();
        }
    } catch (err) {
        // API key check failed — fall through to 401
    }

    res.set('WWW-Authenticate', `Bearer resource_metadata="${getResourceMetadataUrl(req)}"`);
    return res.status(401).json({
        error: 'invalid_token',
        error_description: 'Invalid token or API key'
    });
}

module.exports = mcpAuth;
