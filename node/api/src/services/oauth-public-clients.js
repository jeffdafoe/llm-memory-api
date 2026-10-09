// Public OAuth clients for the claude.ai connector (LLM-733): clients that
// prove nothing at the token endpoint, so the user logs in on /authorize
// instead. Two kinds, both PKCE-only:
//
//   CIMD (Client ID Metadata Document): the client_id is an HTTPS URL the
//   client hosts. claude.ai uses this when our authorization-server metadata
//   advertises client_id_metadata_document_supported plus the "none" auth
//   method. The draft has the server fetch that URL to read the client's
//   redirect_uris; we do not. We accept only client_id URLs on Anthropic's own
//   origins and check redirect_uri against our fixed allowlist, so the document
//   could add nothing, and fetching would mean an outbound request to a URL a
//   caller chose.
//
//   DCR (Dynamic Client Registration, RFC 7591): claude.ai's fallback. It
//   registers a new client on every fresh connection, so instead of storing
//   registrations the client_id carries its own registration, signed:
//   "dcr." + base64url(JSON {r: redirect_uris, t: issued_at}) + "." + HMAC.
//   Nothing to store, nothing to clean up, and it survives restarts.
//
// Agent-name client_ids (the older Client ID / Client Secret path) can never
// collide with either: agent names are [a-z0-9_-] only.

const crypto = require('crypto');
const config = require('./config');

const CIMD_ORIGINS = ['https://claude.ai', 'https://claude.com'];
const DCR_PREFIX = 'dcr.';
// Keeps a DCR signature from ever matching an HMAC made with the same secret
// for something else (the per-agent bearer token).
const DCR_SIGNING_LABEL = 'llm-memory oauth dcr client_id v1\n';

function isCimdClientId(clientId) {
    if (typeof clientId !== 'string' || clientId.length > 512) {
        return false;
    }
    let url;
    try {
        url = new URL(clientId);
    } catch (err) {
        return false;
    }
    if (url.username || url.password || url.hash) {
        return false;
    }
    return CIMD_ORIGINS.includes(url.origin) && url.pathname.length > 1;
}

function signingSecret() {
    const secret = config.get('mcp_oauth_bearer_secret');
    if (!secret) {
        throw new Error('mcp_oauth_bearer_secret is not configured');
    }
    return secret;
}

function sign(payload) {
    return crypto.createHmac('sha256', signingSecret())
        .update(DCR_SIGNING_LABEL + payload)
        .digest('base64url');
}

// redirectUris must already be checked against the allowlist by the caller.
function issueDcrClientId(redirectUris, issuedAtSeconds) {
    const payload = Buffer.from(JSON.stringify({ r: redirectUris, t: issuedAtSeconds })).toString('base64url');
    return DCR_PREFIX + payload + '.' + sign(payload);
}

// Returns { redirectUris, issuedAt } for a client_id this server signed, or null.
function verifyDcrClientId(clientId) {
    if (typeof clientId !== 'string' || !clientId.startsWith(DCR_PREFIX) || clientId.length > 2048) {
        return null;
    }
    const parts = clientId.slice(DCR_PREFIX.length).split('.');
    if (parts.length !== 2) {
        return null;
    }
    const [payload, signature] = parts;
    const expected = Buffer.from(sign(payload));
    const given = Buffer.from(signature);
    // timingSafeEqual throws on unequal lengths, so check length first.
    if (given.length !== expected.length || !crypto.timingSafeEqual(given, expected)) {
        return null;
    }
    let decoded;
    try {
        decoded = JSON.parse(Buffer.from(payload, 'base64url').toString('utf8'));
    } catch (err) {
        return null;
    }
    if (!decoded || !Array.isArray(decoded.r) || !decoded.r.every((uri) => typeof uri === 'string')) {
        return null;
    }
    return { redirectUris: decoded.r, issuedAt: decoded.t };
}

// 'cimd' | 'dcr' | null — null means "not a public client" (an agent name, or garbage).
function publicClientKind(clientId) {
    if (isCimdClientId(clientId)) {
        return 'cimd';
    }
    if (verifyDcrClientId(clientId)) {
        return 'dcr';
    }
    return null;
}

module.exports = {
    CIMD_ORIGINS,
    isCimdClientId,
    issueDcrClientId,
    verifyDcrClientId,
    publicClientKind
};
