// sanitize.js — Input sanitization helpers.
// Shared across routes, middleware, and services.

// Normalize an agent name: trim, lowercase, strip everything except
// ASCII alphanumeric, hyphen, and underscore.  Whitelist-based so it
// catches XSS, injection, Unicode homoglyphs, zero-width chars, etc.
function agentName(name) {
    if (typeof name !== 'string') return name;
    return name.trim().toLowerCase().replace(/[^a-z0-9_-]/g, '');
}

module.exports = { agentName };
