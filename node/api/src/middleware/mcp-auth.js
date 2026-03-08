// Validates bearer tokens on the /mcp endpoint.
// Accepts two token types (tried in order):
//   1. HMAC OAuth tokens — format "agent:hmac_hex", issued by /oauth/token.
//      Deterministic and never expire. Verified by recomputing HMAC. Fast path.
//   2. API keys from agent_api_keys table (for Claude Code direct auth)
// Sets req.mcpAgent and req.mcpPermissions for downstream handlers.

const crypto = require('crypto');
const pool = require('../db');
const config = require('../services/config');
const { hash } = require('../services/hashing');

// Opportunistic heartbeat — update last_seen on every authenticated MCP request.
// Also refreshes active_since if already set, so the activity spinner stays alive
// as long as the agent is making tool calls (without requiring explicit re-calls).
function heartbeat(agent) {
    pool.query(
        `UPDATE agents SET last_seen = NOW(),
         active_since = CASE WHEN active_since IS NOT NULL THEN NOW() ELSE active_since END
         WHERE agent = $1`,
        [agent]
    ).catch(() => {});
}

function getResourceMetadataUrl(req) {
    if (process.env.BASE_URL) {
        return `${process.env.BASE_URL}/.well-known/oauth-protected-resource`;
    }
    return `${req.protocol}://${req.get('host')}/.well-known/oauth-protected-resource`;
}

// Fetch permissions for an agent from DB
async function getPermissions(agent) {
    const result = await pool.query(
        'SELECT p.name FROM agent_permissions ap JOIN permissions p ON p.id = ap.permission_id WHERE ap.agent = $1',
        [agent]
    );
    return result.rows.map(r => r.name);
}

// Try HMAC OAuth token auth. Format: "agent:hmac_hex".
// Returns agent name on success, null on failure.
function tryHmacAuth(token) {
    const colonIndex = token.indexOf(':');
    if (colonIndex === -1) return null;

    const agent = token.substring(0, colonIndex);
    const providedHmac = token.substring(colonIndex + 1);

    const secret = config.get('mcp_oauth_bearer_secret');
    const expectedHmac = crypto.createHmac('sha256', secret).update(agent).digest('hex');

    // Timing-safe comparison to prevent timing attacks
    if (providedHmac.length !== expectedHmac.length) return null;
    const match = crypto.timingSafeEqual(Buffer.from(providedHmac), Buffer.from(expectedHmac));
    return match ? agent : null;
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

            const permissions = await getPermissions(row.agent);
            return { agent: row.agent, permissions };
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

    // Try HMAC OAuth token first (fast path — no DB lookup)
    const hmacAgent = tryHmacAuth(token);
    if (hmacAgent) {
        req.mcpAgent = hmacAgent;
        req.mcpPermissions = await getPermissions(hmacAgent);
        heartbeat(hmacAgent);
        return next();
    }

    // Try API key auth (slower path — iterates keys)
    try {
        const result = await tryApiKeyAuth(token);
        if (result) {
            req.mcpAgent = result.agent;
            req.mcpPermissions = result.permissions;
            heartbeat(result.agent);
            return next();
        }
    } catch (err) {
        // API key check failed — fall through
    }

    res.set('WWW-Authenticate', `Bearer resource_metadata="${getResourceMetadataUrl(req)}"`);
    return res.status(401).json({
        error: 'invalid_token',
        error_description: 'Invalid token or API key'
    });
}

module.exports = mcpAuth;
