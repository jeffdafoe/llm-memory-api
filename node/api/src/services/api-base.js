// Base URL of each outside API the server calls (LLM-906). Production sets
// none of these variables and gets the origins below. The contract tests
// (LLM-902) set them so every provider and the embeddings call go to a local
// fake server. A base may carry a path prefix (http://127.0.0.1:9100/xai), so
// one fake server can tell OpenAI's /v1/responses from xAI's. The call paths
// stay in each caller; only the part in front of them comes from here.

const DEFAULTS = {
    OPENAI_API_BASE: 'https://api.openai.com',
    ANTHROPIC_API_BASE: 'https://api.anthropic.com',
    GOOGLE_API_BASE: 'https://generativelanguage.googleapis.com',
    OPENROUTER_API_BASE: 'https://openrouter.ai',
    PERPLEXITY_API_BASE: 'https://api.perplexity.ai',
    XAI_API_BASE: 'https://api.x.ai'
};

// Read on every call, not once at require time, so a test can set the
// variable after the provider module is loaded.
function apiBase(name) {
    if (!Object.prototype.hasOwnProperty.call(DEFAULTS, name)) {
        throw new Error(`Unknown API base variable ${name}`);
    }
    const raw = (process.env[name] || '').trim();
    if (raw === '') {
        return DEFAULTS[name];
    }

    // The errors never echo the value: a mistyped one can hold an API key or
    // a password, and the message goes to the journal.
    let parsed;
    try {
        parsed = new URL(raw);
    } catch (err) {
        throw new Error(`${name} is not a valid URL`);
    }
    if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') {
        throw new Error(`${name} must be an http or https URL`);
    }
    // The caller appends its path, so a query or fragment would end up in the
    // middle of the URL. The raw check also catches a bare "?" or "#", which
    // URL reports as an empty search or hash. fetch refuses URLs with
    // credentials in them.
    if (raw.includes('?') || raw.includes('#') || parsed.username !== '' || parsed.password !== '') {
        throw new Error(`${name} must not have a query, fragment or credentials`);
    }
    return raw.replace(/\/+$/, '');
}

// Called once at server start, so a bad value stops the server with a clear
// message instead of failing every provider call later.
function checkApiBases() {
    for (const name of Object.keys(DEFAULTS)) {
        apiBase(name);
    }
}

module.exports = { apiBase, checkApiBases, DEFAULTS };
