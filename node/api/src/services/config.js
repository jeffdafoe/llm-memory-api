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

// Get a config value with a fallback default. Returns defaultValue if key not found.
function getOptional(key, defaultValue) {
    if (key in cache) return cache[key];
    return defaultValue;
}

module.exports = { init, get, getOptional };
