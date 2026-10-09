// OAuth 2.1 endpoints for MCP client authentication.
// Supports two grant types:
//   1. client_credentials — for machine-to-machine (Claude Code, scripts)
//   2. authorization_code with PKCE — for browser-based clients (claude.ai)
// authorization_code has two kinds of client:
//   - agent-name client_id + API key as client_secret (the connector's
//     Advanced settings). /authorize shows a connect page, then auto-approves;
//     the secret proves identity at the token endpoint.
//   - public client (LLM-733): the connector added with only the URL. claude.ai
//     identifies itself by CIMD or registers by DCR (services/oauth-public-clients.js),
//     has no secret, and the user logs in on /authorize with agent name +
//     password. The code is bound to the agent that logged in.
// Returns deterministic HMAC-based tokens (not JWTs) that never expire server-side.
// Format: "agent:hmac_hex" — same token every time for a given agent.

const express = require('express');
const { Router } = require('express');
const crypto = require('crypto');
const pool = require('../db');
const config = require('../services/config');
const sanitize = require('../sanitize');
const { log, logError } = require('../services/logger');
const { findApiKeyByToken } = require('../services/api-keys');
const { verifyPasswordLogin } = require('../services/password-login');
const { createAttemptLimiter, acquireAttempt } = require('../services/attempt-limiter');
const { publicClientKind, verifyDcrClientId, issueDcrClientId, ALLOWED_REDIRECT_URIS } = require('../services/oauth-public-clients');
const { renderConnectPage, renderLoginPage } = require('./oauth-connect-page');

const router = Router();

const TOKEN_TTL_SECONDS = 86400; // 24 hours (cosmetic — token never actually expires server-side)

// Password guessing on the connector login: at most 5 tries per agent name and
// 20 per client address in 15 minutes. Every try counts, right or wrong. The
// per-name cap means someone can lock a name out for 15 minutes; that is the
// accepted cost of not letting them guess at it.
const LOGIN_WINDOW_MS = 15 * 60 * 1000;
const loginAgentLimiter = createAttemptLimiter({ maxAttempts: 5, windowMs: LOGIN_WINDOW_MS });
const loginAddressLimiter = createAttemptLimiter({ maxAttempts: 20, windowMs: LOGIN_WINDOW_MS });

// The caller's address. nginx appends the real client to X-Forwarded-For, so
// the LAST hop is the one nginx saw; earlier hops are whatever the client sent.
// Same rule as middleware/request-log.js.
function clientAddress(req) {
    const forwarded = req.headers['x-forwarded-for'];
    if (forwarded) {
        const hops = String(forwarded).split(',');
        const last = hops[hops.length - 1].trim();
        if (last) {
            return last;
        }
    }
    return req.ip || 'unknown';
}

// Pages that take a password must not be framed (clickjacking) or cached.
function setPageHeaders(res) {
    res.set('Cache-Control', 'no-store');
    res.set('X-Frame-Options', 'DENY');
    res.set('Content-Security-Policy', "frame-ancestors 'none'");
}

// In-memory store for authorization codes. Codes expire after 60 seconds.
// Map of code -> { clientId, redirectUri, codeChallenge, codeChallengeMethod, expiresAt }
const authCodes = new Map();
const AUTH_CODE_TTL_MS = 60000;

// Clean up expired codes periodically. unref() so this timer alone does not
// keep a process alive (the HTTP server does that in production; tests exit).
setInterval(() => {
    const now = Date.now();
    for (const [code, data] of authCodes) {
        if (now > data.expiresAt) authCodes.delete(code);
    }
}, 30000).unref();

function getBaseUrl(req) {
    if (process.env.BASE_URL) {
        return process.env.BASE_URL;
    }
    return `${req.protocol}://${req.get('host')}`;
}

// Validate a client_id + client_secret pair against agent_api_keys, via the
// shared indexed lookup (services/api-keys.js, MEM-136) scoped to the named
// actor's keys — one PBKDF2 verify instead of a per-key scan. last_used_at
// is stamped by the service.
// Returns the agent name on success, null on failure.
async function validateClientCredentials(clientId, clientSecret) {
    const { resolveByName } = require('../services/actors');
    const actor = await resolveByName(clientId);
    if (!actor) return null;

    const row = await findApiKeyByToken(clientSecret, { actorId: actor.id });
    if (!row) {
        return null;
    }
    return clientId;
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

// RFC 9728 section 3.1 path form for the /mcp resource. Clients try this one
// first (claude.ai does), and its `resource` is the exact URL the user enters.
router.get('/.well-known/oauth-protected-resource/mcp', (req, res) => {
    const baseUrl = getBaseUrl(req);
    res.json({
        resource: `${baseUrl}/mcp`,
        authorization_servers: [baseUrl],
        bearer_methods_supported: ['header']
    });
});

// RFC 8414 — Authorization Server Metadata
// Tells MCP clients about available endpoints and supported grants.
// claude.ai uses CIMD only when this advertises BOTH
// client_id_metadata_document_supported and "none" as a token auth method
// (its CIMD client is public); otherwise it falls back to registration_endpoint.
router.get('/.well-known/oauth-authorization-server', (req, res) => {
    const baseUrl = getBaseUrl(req);
    res.json({
        issuer: baseUrl,
        authorization_endpoint: `${baseUrl}/authorize`,
        token_endpoint: `${baseUrl}/oauth/token`,
        registration_endpoint: `${baseUrl}/oauth/register`,
        token_endpoint_auth_methods_supported: ['none', 'client_secret_post'],
        grant_types_supported: ['authorization_code', 'client_credentials'],
        response_types_supported: ['code'],
        code_challenge_methods_supported: ['S256'],
        client_id_metadata_document_supported: true
    });
});

// RFC 7591 — Dynamic Client Registration (LLM-733), claude.ai's fallback when
// it does not use CIMD. Registers a public client (PKCE, no secret) for our
// allowed callbacks only; anything else is refused. Nothing is stored: the
// client_id carries its own signed registration (services/oauth-public-clients.js).
// Not /register — that path is the signup page. Body is JSON (express.json is
// mounted app-wide in server.js), as RFC 7591 requires.
router.post('/oauth/register', (req, res) => {
    res.set('Cache-Control', 'no-store');
    const body = req.body || {};
    const redirectUris = body.redirect_uris;
    if (!Array.isArray(redirectUris) || redirectUris.length === 0 || redirectUris.length > ALLOWED_REDIRECT_URIS.length) {
        return res.status(400).json({
            error: 'invalid_redirect_uri',
            error_description: 'redirect_uris must list one or more allowed callback URLs'
        });
    }
    if (!redirectUris.every((uri) => ALLOWED_REDIRECT_URIS.includes(uri))) {
        return res.status(400).json({
            error: 'invalid_redirect_uri',
            error_description: 'redirect_uris may only contain: ' + ALLOWED_REDIRECT_URIS.join(', ')
        });
    }
    let clientName = 'Claude';
    if (typeof body.client_name === 'string' && body.client_name.trim()) {
        clientName = body.client_name.trim().slice(0, 100);
    }
    const issuedAt = Math.floor(Date.now() / 1000);
    let clientId;
    try {
        clientId = issueDcrClientId([...new Set(redirectUris)], issuedAt);
    } catch (err) {
        logError('oauth', 'register', { message: err.message, detail: err.stack, statusCode: 500 });
        return res.status(500).json({ error: 'server_error', error_description: 'Registration failed' });
    }
    // Requested values we do not grant (a secret auth method, refresh_token)
    // are replaced in the response, which RFC 7591 section 3.2.1 allows.
    res.status(201).json({
        client_id: clientId,
        client_id_issued_at: issuedAt,
        client_name: clientName,
        redirect_uris: [...new Set(redirectUris)],
        grant_types: ['authorization_code'],
        response_types: ['code'],
        token_endpoint_auth_method: 'none'
    });
});

// Authorization endpoint — starts the authorization_code flow.
// Claude.ai opens this in the user's browser. We auto-approve and redirect
// back with a code, since the client credentials (configured in claude.ai
// connector settings) already prove identity. No login page needed — but
// the user first passes through the connect page (oauth-connect-page.js).
// Checks shared by GET and POST /authorize. Returns { error } (an OAuth error
// body for a 400) or { publicKind } — 'cimd' / 'dcr' for a public client, null
// for an agent-name client_id. Errors are shown here rather than redirected to
// redirect_uri, which is not trusted until it has passed these checks.
function checkAuthorizeRequest(params) {
    const { response_type, client_id, redirect_uri, code_challenge, code_challenge_method } = params;

    if (response_type !== 'code') {
        return { error: { error: 'unsupported_response_type', error_description: 'Only response_type=code is supported' } };
    }
    if (typeof client_id !== 'string' || !client_id || typeof redirect_uri !== 'string' || !redirect_uri) {
        return { error: { error: 'invalid_request', error_description: 'client_id and redirect_uri are required' } };
    }
    if (!ALLOWED_REDIRECT_URIS.includes(redirect_uri)) {
        return { error: { error: 'invalid_request', error_description: 'redirect_uri is not allowed' } };
    }
    if (typeof code_challenge !== 'string' || !code_challenge || code_challenge_method !== 'S256') {
        return { error: { error: 'invalid_request', error_description: 'code_challenge with S256 method is required' } };
    }
    const publicKind = publicClientKind(client_id);
    // A DCR client may only use the callbacks it registered.
    if (publicKind === 'dcr' && !verifyDcrClientId(client_id).redirectUris.includes(redirect_uri)) {
        return { error: { error: 'invalid_request', error_description: 'redirect_uri was not registered for this client' } };
    }
    return { publicKind };
}

// The authorize parameters the login form carries through its POST.
function oauthFields(params) {
    return {
        response_type: params.response_type,
        client_id: params.client_id,
        redirect_uri: params.redirect_uri,
        state: params.state,
        code_challenge: params.code_challenge,
        code_challenge_method: params.code_challenge_method
    };
}

function mintCodeAndRedirect(res, status, { clientId, redirectUri, codeChallenge, codeChallengeMethod, state, agent }) {
    const code = crypto.randomBytes(32).toString('hex');
    authCodes.set(code, {
        clientId,
        redirectUri,
        codeChallenge,
        codeChallengeMethod,
        // Set only for a public client: the agent who logged in. The token
        // endpoint issues that agent's token and asks for no secret.
        agent: agent || null,
        expiresAt: Date.now() + AUTH_CODE_TTL_MS
    });
    const redirectUrl = new URL(redirectUri);
    redirectUrl.searchParams.set('code', code);
    if (typeof state === 'string' && state) {
        redirectUrl.searchParams.set('state', state);
    }
    res.redirect(status, redirectUrl.toString());
}

router.get('/authorize', async (req, res) => {
    const {
        client_id, redirect_uri, state,
        code_challenge, code_challenge_method
    } = req.query;

    const checked = checkAuthorizeRequest(req.query);
    if (checked.error) {
        return res.status(400).json(checked.error);
    }

    // A connector added with only the URL: ask who is connecting.
    if (checked.publicKind) {
        setPageHeaders(res);
        return res.type('html').send(renderLoginPage({
            clientHost: new URL(redirect_uri).host,
            oauthFields: oauthFields(req.query)
        }));
    }

    // Verify the agent exists and is active
    const { resolveByName } = require('../services/actors');
    const actor = await resolveByName(client_id);
    let agentResult = { rows: [] };
    if (actor) {
        agentResult = await pool.query(
            'SELECT ac.name AS agent, ac.status FROM actors ac WHERE ac.id = $1',
            [actor.id]
        );
    }

    if (agentResult.rows.length === 0) {
        return res.status(400).json({
            error: 'invalid_request',
            error_description: 'Unknown or inactive agent'
        });
    }

    // First pass shows the connect page; its Continue link is this same URL
    // plus confirmed=1, which falls through to issue the code. The code is
    // only minted on the second pass because it lives 60 seconds, less than
    // a user may spend on the page.
    if (req.query.confirmed !== '1') {
        const continueUrl = new URL(req.originalUrl, 'http://placeholder');
        continueUrl.searchParams.set('confirmed', '1');
        setPageHeaders(res);
        return res.type('html').send(renderConnectPage({
            agent: agentResult.rows[0].agent,
            continueUrl: continueUrl.pathname + continueUrl.search
        }));
    }

    mintCodeAndRedirect(res, 302, {
        clientId: client_id,
        redirectUri: redirect_uri,
        codeChallenge: code_challenge,
        codeChallengeMethod: code_challenge_method,
        state
    });
});

// The login form's POST (public clients only). Re-checks the whole OAuth
// request — the hidden fields came back from the browser and are untrusted —
// then the rate limits, then the password. A failure re-renders the form with
// the agent name kept; the password is never echoed.
router.post('/authorize', express.urlencoded({ extended: false }), async (req, res) => {
    const body = req.body || {};
    const checked = checkAuthorizeRequest(body);
    if (checked.error) {
        return res.status(400).json(checked.error);
    }
    if (!checked.publicKind) {
        return res.status(400).json({
            error: 'invalid_request',
            error_description: 'Login is only for connectors added without a client secret'
        });
    }

    const agentInput = typeof body.agent === 'string' ? body.agent : '';
    const agent = sanitize.agentName(agentInput);
    const password = typeof body.password === 'string' ? body.password : '';
    const showForm = (status, error) => {
        setPageHeaders(res);
        return res.status(status).type('html').send(renderLoginPage({
            clientHost: new URL(body.redirect_uri).host,
            oauthFields: oauthFields(body),
            agentValue: agentInput.slice(0, 64),
            error
        }));
    };

    if (!agent || !password) {
        return showForm(400, 'Enter your agent name and password.');
    }

    const address = clientAddress(req);
    const attempt = acquireAttempt([[loginAgentLimiter, agent], [loginAddressLimiter, address]]);
    if (!attempt.allowed) {
        const minutes = Math.max(1, Math.ceil(attempt.retryAfterSeconds / 60));
        res.set('Retry-After', String(attempt.retryAfterSeconds));
        log('oauth', 'connector_login_limited', { agent, address });
        return showForm(429, `Too many tries. Wait ${minutes} minute${minutes === 1 ? '' : 's'} and try again.`);
    }

    let login;
    try {
        login = await verifyPasswordLogin(agent, password);
    } catch (err) {
        logError('oauth', 'connector-login', { agent, message: err.message, detail: err.stack, statusCode: 500 });
        return showForm(500, 'Something went wrong. Try again.');
    }
    if (!login) {
        log('oauth', 'connector_login_failed', { agent, address });
        return showForm(401, 'Wrong agent name or password.');
    }

    log('oauth', 'connector_login', { agent: login.name, client_kind: checked.publicKind });
    // 303: the browser follows a POST's redirect with a GET.
    mintCodeAndRedirect(res, 303, {
        clientId: body.client_id,
        redirectUri: body.redirect_uri,
        codeChallenge: body.code_challenge,
        codeChallengeMethod: body.code_challenge_method,
        state: body.state,
        agent: login.name
    });
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
        logError('oauth', 'client-credentials', { agent: req.body.client_id, message: err.message, detail: err.stack, statusCode: 500 });
        res.status(500).json({
            error: 'server_error',
            error_description: 'Token generation failed'
        });
    }
}

// authorization_code grant — browser-based auth for claude.ai
async function handleAuthorizationCode(req, res) {
    const { code, code_verifier, client_id, client_secret, redirect_uri } = req.body;

    // client_secret is checked below, once the code says which kind of client
    // this is: a public client (LLM-733) has none.
    if (!code || !code_verifier || !client_id || !redirect_uri) {
        return res.status(400).json({
            error: 'invalid_request',
            error_description: 'code, code_verifier, client_id, and redirect_uri are required'
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
    if (redirect_uri !== codeData.redirectUri) {
        return res.status(400).json({
            error: 'invalid_grant',
            error_description: 'redirect_uri mismatch'
        });
    }

    // Validate client_id matches
    if (client_id !== codeData.clientId) {
        return res.status(400).json({
            error: 'invalid_grant',
            error_description: 'client_id mismatch'
        });
    }

    // Validate PKCE code_verifier (always required)
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

    // Public client: the user proved who they are by logging in on /authorize,
    // and PKCE above proves this request comes from whoever started that flow.
    // The token is for the agent bound into the code — never one the client names.
    if (codeData.agent) {
        try {
            return res.json({
                access_token: issueOAuthToken(codeData.agent),
                token_type: 'Bearer',
                expires_in: TOKEN_TTL_SECONDS
            });
        } catch (err) {
            logError('oauth', 'authorization-code', { agent: codeData.agent, message: err.message, detail: err.stack, statusCode: 500 });
            return res.status(500).json({
                error: 'server_error',
                error_description: 'Token generation failed'
            });
        }
    }

    if (!client_secret) {
        return res.status(401).json({
            error: 'invalid_client',
            error_description: 'client_secret is required'
        });
    }

    try {
        // Validate client_secret (always required)
        const agent = await validateClientCredentials(codeData.clientId, client_secret);
        if (!agent) {
            return res.status(401).json({
                error: 'invalid_client',
                error_description: 'Invalid client_secret'
            });
        }

        const token = issueOAuthToken(codeData.clientId);

        res.json({
            access_token: token,
            token_type: 'Bearer',
            expires_in: TOKEN_TTL_SECONDS
        });
    } catch (err) {
        logError('oauth', 'authorization-code', { agent: req.body.client_id, message: err.message, detail: err.stack, statusCode: 500 });
        res.status(500).json({
            error: 'server_error',
            error_description: 'Token generation failed'
        });
    }
}

// Exposed for oauth.test.js, which checks when codes are minted.
router.authCodes = authCodes;

module.exports = router;
