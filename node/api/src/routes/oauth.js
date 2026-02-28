// OAuth 2.1 discovery and token endpoint for MCP client authentication.
// Implements client_credentials grant using agent API keys from the
// agent_api_keys table. Issues JWTs with embedded permissions.

const express = require('express');
const { Router } = require('express');
const jwt = require('jsonwebtoken');
const pool = require('../db');
const { hash } = require('../services/hashing');

const router = Router();

const JWT_SECRET = process.env.JWT_SECRET;
const TOKEN_TTL_SECONDS = 3600; // 1 hour

function getBaseUrl(req) {
    if (process.env.BASE_URL) {
        return process.env.BASE_URL;
    }
    return `${req.protocol}://${req.get('host')}`;
}

// RFC 9728 — Protected Resource Metadata
// Tells MCP clients where to find the authorization server
router.get('/.well-known/oauth-protected-resource', (req, res) => {
    const baseUrl = getBaseUrl(req);
    res.json({
        resource: baseUrl,
        authorization_servers: [baseUrl],
        bearer_methods_supported: ['header']
    });
});

// RFC 8414 — Authorization Server Metadata
// Tells MCP clients about our token endpoint and supported grants
router.get('/.well-known/oauth-authorization-server', (req, res) => {
    const baseUrl = getBaseUrl(req);
    res.json({
        issuer: baseUrl,
        token_endpoint: `${baseUrl}/oauth/token`,
        token_endpoint_auth_methods_supported: ['client_secret_post'],
        grant_types_supported: ['client_credentials'],
        response_types_supported: [],
        code_challenge_methods_supported: ['S256']
    });
});

// OAuth token endpoint — client_credentials grant.
// client_id = agent name, client_secret = API key.
// Body is application/x-www-form-urlencoded per OAuth spec.
router.post('/oauth/token', express.urlencoded({ extended: false }), async (req, res) => {
    const { grant_type, client_id, client_secret } = req.body;

    if (grant_type !== 'client_credentials') {
        return res.status(400).json({
            error: 'unsupported_grant_type',
            error_description: 'Only client_credentials grant is supported'
        });
    }

    if (!client_id || !client_secret) {
        return res.status(400).json({
            error: 'invalid_request',
            error_description: 'client_id and client_secret are required'
        });
    }

    try {
        // Look up active (non-revoked) API keys for this agent
        const result = await pool.query(
            `SELECT id, key_hash, key_salt FROM agent_api_keys
             WHERE agent = $1 AND revoked_at IS NULL`,
            [client_id]
        );

        let matchedKeyId = null;
        for (const row of result.rows) {
            const computed = hash(client_secret, row.key_salt);
            if (computed === row.key_hash) {
                matchedKeyId = row.id;
                break;
            }
        }

        if (!matchedKeyId) {
            return res.status(401).json({
                error: 'invalid_client',
                error_description: 'Invalid client_id or client_secret'
            });
        }

        // Track usage
        await pool.query(
            'UPDATE agent_api_keys SET last_used_at = NOW() WHERE id = $1',
            [matchedKeyId]
        );

        // Fetch agent permissions
        const permissionResult = await pool.query(
            `SELECT p.name FROM agent_permissions ap
             JOIN permissions p ON p.id = ap.permission_id
             WHERE ap.agent = $1`,
            [client_id]
        );
        const permissions = permissionResult.rows.map(r => r.name);

        // Issue JWT
        const token = jwt.sign(
            { agent: client_id, permissions },
            JWT_SECRET,
            { expiresIn: TOKEN_TTL_SECONDS }
        );

        res.json({
            access_token: token,
            token_type: 'Bearer',
            expires_in: TOKEN_TTL_SECONDS
        });
    } catch (err) {
        console.error('OAuth token error:', err.message);
        res.status(500).json({
            error: 'server_error',
            error_description: 'Token generation failed'
        });
    }
});

module.exports = router;
