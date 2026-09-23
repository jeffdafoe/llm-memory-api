// In-memory failed-attempt limiter (LLM-670). Used by the account-link route,
// which checks another account's password and would otherwise be an
// unthrottled password-guessing endpoint for any logged-in user.
//
// Deliberately in-process (a Map), not Postgres: losing the counters on a
// restart only resets the window early, and nothing else needs to read them.
// Kept free of DB/config imports so it can be unit-tested directly.
//
// Only FAILURES count. A success clears the key, so a user who links several
// of their own accounts is never locked out by their own correct entries.

function createAttemptLimiter({ maxFailures, windowMs, now = Date.now }) {
    // key -> { failures: number, windowStart: ms }
    const entries = new Map();

    // Drop an entry whose window has passed, so the Map cannot grow without
    // bound from one-off keys.
    function current(key) {
        const entry = entries.get(key);
        if (entry && now() - entry.windowStart >= windowMs) {
            entries.delete(key);
            return null;
        }
        return entry || null;
    }

    // Returns { blocked: false } or { blocked: true, retryAfterSeconds }.
    function check(key) {
        const entry = current(key);
        if (entry && entry.failures >= maxFailures) {
            const remainingMs = windowMs - (now() - entry.windowStart);
            return { blocked: true, retryAfterSeconds: Math.max(1, Math.ceil(remainingMs / 1000)) };
        }
        return { blocked: false };
    }

    function recordFailure(key) {
        const entry = current(key);
        if (entry) {
            entry.failures += 1;
        } else {
            entries.set(key, { failures: 1, windowStart: now() });
        }
    }

    function recordSuccess(key) {
        entries.delete(key);
    }

    return { check, recordFailure, recordSuccess };
}

module.exports = { createAttemptLimiter };
