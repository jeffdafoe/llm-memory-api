// Pure rate-limit config merge (LLM-156), split out of virtual-agent.js so it
// can be unit-tested without loading the whole virtual-agent stack — that
// module registers a system handler at load time and pulls in the DB/provider
// chain. virtual-agent.js's effectiveRateLimit() supplies the live globals plus
// the agent's per-agent overrides and delegates the precedence to this merge.

// mergeRateLimit layers an agent's per-agent overrides on top of the global
// defaults. Each override field is independent — an agent may override just the
// limit and inherit the global window/cooldown. Returns a fresh object; the
// caller-supplied globals are not mutated.
function mergeRateLimit(globals, overrides) {
    const out = {
        limit: globals.limit,
        windowMs: globals.windowMs,
        cooldownMs: globals.cooldownMs,
    };
    if (overrides) {
        if (overrides.limit != null) out.limit = overrides.limit;
        if (overrides.windowMs != null) out.windowMs = overrides.windowMs;
        if (overrides.cooldownMs != null) out.cooldownMs = overrides.cooldownMs;
    }
    return out;
}

module.exports = { mergeRateLimit };
