// OAuth 2.1 endpoints for MCP client authentication.
// Supports two grant types:
//   1. client_credentials — for machine-to-machine (Claude Code, scripts)
//   2. authorization_code with PKCE — for browser-based clients (claude.ai)
// Both use agent API keys from agent_api_keys table.
// Returns deterministic HMAC-based tokens (not JWTs) that never expire server-side.
// Format: "agent:hmac_hex" — same token every time for a given agent.

const express = require('express');
const { Router } = require('express');
const crypto = require('crypto');
const pool = require('../db');
const config = require('../services/config');
const { hash } = require('../services/hashing');
const { logError } = require('../services/logger');

const router = Router();

const TOKEN_TTL_SECONDS = 86400; // 24 hours (cosmetic — token never actually expires server-side)

// In-memory store for authorization codes. Codes expire after 60 seconds.
// Map of code -> { clientId, redirectUri, codeChallenge, codeChallengeMethod, expiresAt }
const authCodes = new Map();
const AUTH_CODE_TTL_MS = 60000;

// Clean up expired codes periodically
setInterval(() => {
    const now = Date.now();
    for (const [code, data] of authCodes) {
        if (now > data.expiresAt) authCodes.delete(code);
    }
}, 30000);

function getBaseUrl(req) {
    if (process.env.BASE_URL) {
        return process.env.BASE_URL;
    }
    return `${req.protocol}://${req.get('host')}`;
}

// Validate a client_id + client_secret pair against agent_api_keys.
// Returns the agent name on success, null on failure.
async function validateClientCredentials(clientId, clientSecret) {
    const result = await pool.query(
        'SELECT id, key_hash, key_salt FROM agent_api_keys WHERE agent = $1 AND revoked_at IS NULL',
        [clientId]
    );

    for (const row of result.rows) {
        const computed = hash(clientSecret, row.key_salt);
        if (computed === row.key_hash) {
            // Track usage
            pool.query('UPDATE agent_api_keys SET last_used_at = NOW() WHERE id = $1', [row.id]).catch(() => {});
            return clientId;
        }
    }
    return null;
}

// Issue a deterministic HMAC token for an agent.
// Same agent always gets the same token. Token never expires server-side.
// Format: "agent:hmac_hex" — mcp-auth verifies by recomputing the HMAC.
function issueOAuthToken(agent) {
    const secret = config.get('mcp_oauth_bearer_secret');
    const hmac = crypto.createHmac('sha256', secret).update(agent).digest('hex');
    return `${agent}:${hmac}`;
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
// Tells MCP clients about available endpoints and supported grants
router.get('/.well-known/oauth-authorization-server', (req, res) => {
    const baseUrl = getBaseUrl(req);
    res.json({
        issuer: baseUrl,
        authorization_endpoint: `${baseUrl}/authorize`,
        token_endpoint: `${baseUrl}/oauth/token`,
        token_endpoint_auth_methods_supported: ['client_secret_post'],
        grant_types_supported: ['authorization_code', 'client_credentials'],
        response_types_supported: ['code'],
        code_challenge_methods_supported: ['S256']
    });
});

// Authorization endpoint — starts the authorization_code flow.
// Claude.ai opens this in Wendy's browser. We auto-approve and redirect
// back with a code, since the client credentials (configured in claude.ai
// connector settings) already prove identity. No login page needed.
router.get('/authorize', async (req, res) => {
    const {
        response_type, client_id, redirect_uri, state,
        code_challenge, code_challenge_method
    } = req.query;

    if (response_type !== 'code') {
        return res.status(400).json({
            error: 'unsupported_response_type',
            error_description: 'Only response_type=code is supported'
        });
    }

    if (!client_id || !redirect_uri) {
        return res.status(400).json({
            error: 'invalid_request',
            error_description: 'client_id and redirect_uri are required'
        });
    }

    // Verify the agent exists and is active
    const agentResult = await pool.query(
        'SELECT agent, status FROM agents WHERE agent = $1',
        [client_id]
    );

    if (agentResult.rows.length === 0 || agentResult.rows[0].status !== 'active') {
        return res.status(400).json({
            error: 'invalid_request',
            error_description: 'Unknown or inactive agent'
        });
    }

    // Generate authorization code
    const code = crypto.randomBytes(32).toString('hex');
    authCodes.set(code, {
        clientId: client_id,
        redirectUri: redirect_uri,
        codeChallenge: code_challenge || null,
        codeChallengeMethod: code_challenge_method || null,
        expiresAt: Date.now() + AUTH_CODE_TTL_MS
    });

    // Redirect back to the client with the code
    const redirectUrl = new URL(redirect_uri);
    redirectUrl.searchParams.set('code', code);
    if (state) redirectUrl.searchParams.set('state', state);

    res.redirect(302, redirectUrl.toString());
});

// OAuth token endpoint — handles both grant types.
// client_credentials: client_id + client_secret → token directly
// authorization_code: code + code_verifier → token (with PKCE validation)
router.post('/oauth/token', express.urlencoded({ extended: false }), async (req, res) => {
    const { grant_type } = req.body;

    if (grant_type === 'client_credentials') {
        return handleClientCredentials(req, res);
    } else if (grant_type === 'authorization_code') {
        return handleAuthorizationCode(req, res);
    } else {
        return res.status(400).json({
            error: 'unsupported_grant_type',
            error_description: 'Supported grant types: authorization_code, client_credentials'
        });
    }
});

// client_credentials grant — machine-to-machine auth for Claude Code
async function handleClientCredentials(req, res) {
    const { client_id, client_secret } = req.body;

    if (!client_id || !client_secret) {
        return res.status(400).json({
            error: 'invalid_request',
            error_description: 'client_id and client_secret are required'
        });
    }

    try {
        const agent = await validateClientCredentials(client_id, client_secret);
        if (!agent) {
            return res.status(401).json({
                error: 'invalid_client',
                error_description: 'Invalid client_id or client_secret'
            });
        }

        const token = issueOAuthToken(agent);

        res.json({
            access_token: token,
            token_type: 'Bearer',
            expires_in: TOKEN_TTL_SECONDS
        });
    } catch (err) {
        logError('oauth', 'client-credentials', { agent: req.body.client_id, message: err.message, detail: err.stack });
        res.status(500).json({
            error: 'server_error',
            error_description: 'Token generation failed'
        });
    }
}

// authorization_code grant — browser-based auth for claude.ai
async function handleAuthorizationCode(req, res) {
    const { code, code_verifier, client_id, client_secret, redirect_uri } = req.body;

    if (!code) {
        return res.status(400).json({
            error: 'invalid_request',
            error_description: 'code is required'
        });
    }

    // Look up and consume the authorization code (one-time use)
    const codeData = authCodes.get(code);
    authCodes.delete(code);

    if (!codeData || Date.now() > codeData.expiresAt) {
        return res.status(400).json({
            error: 'invalid_grant',
            error_description: 'Invalid or expired authorization code'
        });
    }

    // Validate redirect_uri matches what was used in /authorize
    if (redirect_uri && redirect_uri !== codeData.redirectUri) {
        return res.status(400).json({
            error: 'invalid_grant',
            error_description: 'redirect_uri mismatch'
        });
    }

    // Validate client_id matches
    if (client_id && client_id !== codeData.clientId) {
        return res.status(400).json({
            error: 'invalid_grant',
            error_description: 'client_id mismatch'
        });
    }

    // Validate PKCE code_verifier if a code_challenge was provided during /authorize
    if (codeData.codeChallenge) {
        if (!code_verifier) {
            return res.status(400).json({
                error: 'invalid_request',
                error_description: 'code_verifier is required'
            });
        }

        // S256: BASE64URL(SHA256(code_verifier)) should equal code_challenge
        const computed = crypto.createHash('sha256')
            .update(code_verifier)
            .digest('base64url');

        if (computed !== codeData.codeChallenge) {
            return res.status(400).json({
                error: 'invalid_grant',
                error_description: 'PKCE code_verifier validation failed'
            });
        }
    }

    try {
        // If client_secret is provided, validate it (confidential client)
        if (client_secret) {
            const agent = await validateClientCredentials(codeData.clientId, client_secret);
            if (!agent) {
                return res.status(401).json({
                    error: 'invalid_client',
                    error_description: 'Invalid client_secret'
                });
            }
        }

        const token = issueOAuthToken(codeData.clientId);

        res.json({
            access_token: token,
            token_type: 'Bearer',
            expires_in: TOKEN_TTL_SECONDS
        });
    } catch (err) {
        logError('oauth', 'authorization-code', { agent: req.body.client_id, message: err.message, detail: err.stack });
        res.status(500).json({
            error: 'server_error',
            error_description: 'Token generation failed'
        });
    }
}

module.exports = router;
