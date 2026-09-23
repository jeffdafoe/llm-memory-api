// In-memory attempt limiter (LLM-670). Used by the account-link route, which
// checks another account's password and would otherwise be an unthrottled
// password-guessing endpoint for any logged-in user.
//
// Deliberately in-process (a Map), not Postgres or Valkey: memory-api runs as
// ONE node process (systemd memory-api.service -> node src/server.js), so the
// counters are already global. Losing them on a restart only resets the
// window early. If the API is ever run as several processes, this must move
// to a shared store — each process would otherwise allow its own quota.
// Kept free of DB/config imports so it can be unit-tested directly.
//
// Attempts are COUNTED when they start (acquireAttempt), not when they fail.
// Counting only after the password check would let a burst of parallel
// requests all pass the check before the first failure is recorded. A
// success clears the key, so a user who links several of their own accounts
// is never locked out by their own correct entries.

function createAttemptLimiter({ maxAttempts, windowMs, maxKeys = 10000, sweepAt = 1000, now = Date.now }) {
    // key -> { attempts: number, windowStart: ms }
    const entries = new Map();

    function isExpired(entry) {
        return now() - entry.windowStart >= windowMs;
    }

    function current(key) {
        const entry = entries.get(key);
        if (entry && isExpired(entry)) {
            entries.delete(key);
            return null;
        }
        return entry || null;
    }

    // Expired keys are otherwise only removed when the same key comes back,
    // and one-off keys (a guessed username) never do. Sweep the whole Map once
    // it passes sweepAt, so it holds at most the keys active in one window.
    function sweep() {
        for (const [key, entry] of entries) {
            if (isExpired(entry)) {
                entries.delete(key);
            }
        }
    }

    // Returns { blocked: false } or { blocked: true, retryAfterSeconds }.
    // Does not change state.
    function check(key) {
        const entry = current(key);
        if (entry) {
            if (entry.attempts >= maxAttempts) {
                const remainingMs = windowMs - (now() - entry.windowStart);
                return { blocked: true, retryAfterSeconds: Math.max(1, Math.ceil(remainingMs / 1000)) };
            }
            return { blocked: false };
        }
        // A new key while the Map is full (a flood of distinct keys that the
        // sweep could not reclaim): refuse rather than grow without bound.
        if (entries.size >= sweepAt) {
            sweep();
        }
        if (entries.size >= maxKeys) {
            return { blocked: true, retryAfterSeconds: Math.max(1, Math.ceil(windowMs / 1000)) };
        }
        return { blocked: false };
    }

    function consume(key) {
        const entry = current(key);
        if (entry) {
            entry.attempts += 1;
        } else {
            entries.set(key, { attempts: 1, windowStart: now() });
        }
    }

    function reset(key) {
        entries.delete(key);
    }

    return { check, consume, reset, size: () => entries.size };
}

// Reserve one attempt against several limiters at once: [[limiter, key], ...].
// All are checked first and only then all consumed, with no await in between,
// so concurrent requests cannot both pass on the same remaining quota (Node
// runs this synchronously). Nothing is consumed when any limiter is blocked.
// Returns { allowed: true } or { allowed: false, retryAfterSeconds }.
function acquireAttempt(pairs) {
    let retryAfterSeconds = 0;
    for (const [limiter, key] of pairs) {
        const result = limiter.check(key);
        if (result.blocked) {
            retryAfterSeconds = Math.max(retryAfterSeconds, result.retryAfterSeconds);
        }
    }
    if (retryAfterSeconds > 0) {
        return { allowed: false, retryAfterSeconds };
    }
    for (const [limiter, key] of pairs) {
        limiter.consume(key);
    }
    return { allowed: true };
}

module.exports = { createAttemptLimiter, acquireAttempt };
