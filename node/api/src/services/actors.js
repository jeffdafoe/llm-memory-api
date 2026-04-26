// Actor resolution service — maps between agent names and integer actor IDs.
// All external APIs continue to use agent names; this service handles the
// internal translation to/from actors.id for database queries.
//
// Actor identity is capability-based (no type column):
//   - Has agent_configuration row → can authenticate as agent
//   - Has password_hash → can authenticate as web user
//   - Has both → dual identity (e.g. Wendy)

const pool = require('../db');

// In-memory cache with TTL. Actor records rarely change, so caching is safe.
const nameCache = new Map(); // name -> { actor, expires }
const idCache = new Map();   // id -> { actor, expires }
const CACHE_TTL_MS = 5 * 60 * 1000;

async function resolveByName(name) {
    const cached = nameCache.get(name);
    if (cached && cached.expires > Date.now()) return cached.actor;

    const result = await pool.query('SELECT id, name FROM actors WHERE name = $1', [name]);
    if (result.rows.length === 0) return null;

    const actor = result.rows[0];
    cacheActor(actor);
    return actor;
}

async function resolveById(id) {
    const cached = idCache.get(id);
    if (cached && cached.expires > Date.now()) return cached.actor;

    const result = await pool.query('SELECT id, name FROM actors WHERE id = $1', [id]);
    if (result.rows.length === 0) return null;

    const actor = result.rows[0];
    cacheActor(actor);
    return actor;
}

// Resolve multiple names in a single query. Returns Map<name, actor>.
async function resolveMultipleByName(names) {
    const result = new Map();
    const uncached = [];

    for (const name of names) {
        const cached = nameCache.get(name);
        if (cached && cached.expires > Date.now()) {
            result.set(name, cached.actor);
        } else {
            uncached.push(name);
        }
    }

    if (uncached.length > 0) {
        const rows = await pool.query('SELECT id, name FROM actors WHERE name = ANY($1)', [uncached]);
        for (const row of rows.rows) {
            cacheActor(row);
            result.set(row.name, row);
        }
    }

    return result;
}

// Require an actor to exist by name. Throws 404 if not found.
async function requireByName(name) {
    const actor = await resolveByName(name);
    if (!actor) {
        throw Object.assign(new Error(`Agent "${name}" is not registered`), { statusCode: 404 });
    }
    return actor;
}

function cacheActor(actor) {
    const entry = { actor, expires: Date.now() + CACHE_TTL_MS };
    nameCache.set(actor.name, entry);
    idCache.set(actor.id, entry);
}

function clearCache() {
    nameCache.clear();
    idCache.clear();
}

// Check if an actor can access a virtual agent.
// Access is granted if any of:
//   1. The actor is the creator (owner) of the virtual agent
//   2. The actor has an admin_permissions row (admin)
//   3. There's a virtual_agent_access row with grantee_actor_id = NULL (public)
//   4. There's a virtual_agent_access row with grantee_actor_id = actor's id
//   5. The actor and virtual agent share at least one realm (realms function as
//      VA-access groups — e.g. salem-engine in 'salem' reaches all salem NPCs)
async function canAccessVirtualAgent(actorId, virtualAgentId) {
    const result = await pool.query(`
        SELECT EXISTS(
            -- Creator/owner check
            SELECT 1 FROM actors WHERE id = $2 AND created_by = $1
            UNION ALL
            -- Admin check
            SELECT 1 FROM admin_permissions WHERE actor_id = $1
            UNION ALL
            -- ACL check (public or explicit grant)
            SELECT 1 FROM virtual_agent_access
            WHERE virtual_agent_id = $2
              AND (grantee_actor_id IS NULL OR grantee_actor_id = $1)
            UNION ALL
            -- Realm overlap: caller and target share at least one realm.
            -- Empty realms arrays don't overlap (PG && returns false), so this
            -- only grants access when both sides have explicit realm membership.
            SELECT 1 FROM actors caller, actors target
            WHERE caller.id = $1 AND target.id = $2
              AND caller.realms && target.realms
        ) AS has_access
    `, [actorId, virtualAgentId]);
    return result.rows[0].has_access;
}

// Check if a name is available for use as a new actor/agent.
// Checks against existing actors and existing document namespaces.
async function checkNameAvailability(name) {
    const existing = await resolveByName(name);
    if (existing) {
        return { available: false, reason: 'Name already taken.' };
    }

    // Check against existing namespaces in documents — catches namespaces
    // that don't correspond to an actor (e.g. "shared")
    const nsResult = await pool.query(
        'SELECT 1 FROM documents WHERE namespace = $1 LIMIT 1', [name]
    );
    if (nsResult.rows.length > 0) {
        return { available: false, reason: 'This name conflicts with an existing namespace.' };
    }

    return { available: true };
}

// Send a proposed actor name to the "actor-name-check" virtual agent for
// moderation.  Returns { approved: true } or { approved: false, reason }.
// Gracefully degrades — if the VA doesn't exist, has no API key, or times
// out, the name is approved by default so registration isn't blocked.
const MODERATION_AGENT = 'actor-name-check';
const MODERATION_TIMEOUT_MS = 15000;

async function moderateActorName(name) {
    try {
        // Load the virtual agent's config
        const agentRow = await pool.query(
            `SELECT ac.id AS actor_id, ac.name AS agent, agc.virtual, agc.provider,
                    agc.model, agc.api_key, agc.personality, agc.startup_instructions,
                    agc.max_tokens, agc.temperature, agc.cache_prompts, agc.configuration
             FROM agent_configuration agc
             JOIN actors ac ON ac.id = agc.actor_id
             WHERE ac.name = $1`,
            [MODERATION_AGENT]
        );

        if (agentRow.rows.length === 0 || !agentRow.rows[0].virtual) {
            return { approved: true };  // VA doesn't exist — skip moderation
        }

        const agent = agentRow.rows[0];
        if (!agent.api_key || !agent.provider || !agent.model) {
            return { approved: true };  // VA not configured — skip moderation
        }

        const { createProvider, decryptApiKey } = require('./provider');

        const apiKey = decryptApiKey(agent.api_key);
        const conf = {};
        if (agent.max_tokens) conf.max_tokens = agent.max_tokens;
        if (agent.temperature != null) conf.temperature = agent.temperature;
        if (agent.cache_prompts) conf.cache_prompts = true;
        if (agent.configuration) {
            try {
                Object.assign(conf, JSON.parse(agent.configuration));
            } catch (_) {}
        }

        // Use the VA's personality/instructions as system prompt, with a
        // fallback if instructions haven't been set yet.
        let systemPrompt = agent.startup_instructions || agent.personality || '';
        if (!systemPrompt.trim()) {
            systemPrompt = 'You are a name moderation agent. Evaluate whether the proposed username is appropriate for a professional platform. Reply with exactly APPROVED or REJECTED followed by a brief reason.';
        }

        const providerFn = createProvider(agent.provider, agent.model, apiKey, conf);

        // Race the provider call against a timeout
        const result = await Promise.race([
            providerFn(systemPrompt, `Proposed username: ${name}`),
            new Promise((_, reject) =>
                setTimeout(() => reject(new Error('Moderation timeout')), MODERATION_TIMEOUT_MS)
            )
        ]);

        const response = (result.text || '').trim();

        // Parse response — look for APPROVED or REJECTED at the start
        if (response.toUpperCase().startsWith('APPROVED')) {
            return { approved: true };
        }
        if (response.toUpperCase().startsWith('REJECTED')) {
            // Extract reason after "REJECTED" — skip colon/dash/space separators
            const reason = response.slice(8).replace(/^[\s:—-]+/, '').trim();
            return { approved: false, reason: reason || 'Name not allowed.' };
        }

        // Ambiguous response — approve by default, log it
        const { log } = require('./logger');
        log('actors', 'moderation-ambiguous', { name, response: response.slice(0, 200) });
        return { approved: true };

    } catch (err) {
        // Any failure — log and approve (don't block registration)
        const { log } = require('./logger');
        log('actors', 'moderation-error', { name, error: err.message });
        return { approved: true };
    }
}

module.exports = { resolveByName, resolveById, resolveMultipleByName, requireByName, clearCache, canAccessVirtualAgent, checkNameAvailability, moderateActorName };
