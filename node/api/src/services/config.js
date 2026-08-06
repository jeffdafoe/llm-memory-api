// Loads config values from the `config` table at startup.
// Must be initialized before the server accepts requests.
// After init, values are cached in memory — no DB hit on access.

const pool = require('../db');

const cache = {};

// Load all config rows into memory. Call once at startup.
async function init() {
    const result = await pool.query('SELECT key, value FROM config');
    for (const row of result.rows) {
        cache[row.key] = row.value;
    }
}

// Get a config value. Throws if key not found (catch misconfigs early).
function get(key) {
    if (!(key in cache)) {
        throw new Error(`Config key "${key}" not found. Was config.init() called?`);
    }
    return cache[key];
}

// Update a single cached value after a DB write.
function set(key, value) {
    cache[key] = value;
}

// Parse a config value that must be a non-negative number, falling back when it
// is absent, blank or unparseable.
//
// Use this instead of `parseFloat(value) || fallback` on any key whose
// description gives 0 a meaning ("0 to disable", "0 = no decay"). `||` treats
// the parsed 0 as absent and substitutes the fallback, so the documented
// escape hatch silently does the opposite of what it says (LLM-584).
function parseNonNegativeFinite(value, fallback = 0) {
    const n = Number(value);
    if (Number.isFinite(n) && n >= 0) return n;
    return fallback;
}

module.exports = { init, get, set, parseNonNegativeFinite };
