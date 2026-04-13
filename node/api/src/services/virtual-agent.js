// Virtual agent service — handles AI responses for API-backed agents.
// Three trigger paths:
//   1. Discussion messages/votes — via system handler ('virtual-agent' type)
//   2. Direct chat — when a real agent chats a virtual agent outside a discussion
//   3. Direct mail — when a real agent mails a virtual agent, auto-replies

const pool = require('../db');
const { log, logError } = require('./logger');
const { searchMemory } = require('./memory');
const { createProvider, decryptApiKey, calculateCost, getModelConfigVersion } = require('./provider');
const { broadcast } = require('./events');
const { chatSend } = require('./chat');
const { saveNote, readNote } = require('./documents');
const config = require('./config');
const { requireByName } = require('./actors');

const MIN_ACTIVITY_MS = 3000;

// Format seconds into a human-readable duration string (e.g. "5 minutes", "1 hour 10 minutes")
function formatDuration(totalSeconds) {
    if (totalSeconds < 60) return totalSeconds + ' seconds';
    const hours = Math.floor(totalSeconds / 3600);
    const minutes = Math.ceil((totalSeconds % 3600) / 60);
    if (hours > 0 && minutes > 0) return hours + ' hour(s) ' + minutes + ' minutes';
    if (hours > 0) return hours + ' hour(s)';
    return minutes + ' minutes';
}

// In-memory rate limiter: agent -> array of call timestamps
const callHistory = {};

// --- Virtual agent status lifecycle ---

// Set a virtual agent's status (available, degraded, error).
async function setAgentStatus(agentName, status) {
    await pool.query("UPDATE actors SET status = $1 WHERE name = $2", [status, agentName]);
    broadcast('agent_activity', { agent: agentName, status });
    logVA('status-change', { agent: agentName, status });
}

// Retry a provider call with exponential backoff.
// Returns the result on success, or throws after all retries exhausted.
// Optional onFirstFailure(err, retryInfo) callback fires once after the first
// attempt fails, before the backoff sleep — lets callers send immediate feedback
// (e.g. "retrying, please wait") so the sender isn't left in the dark.
async function retryWithBackoff(agentName, fn, onFirstFailure) {
    // Retry count is derived from the backoff cadence — each entry = one retry.
    // e.g. "300,600,3600" means 3 retries at 5m, 10m, 1h intervals.
    const backoffStr = config.get('virtual_agent_retry_backoff') || '60,600,3600';
    const backoffs = backoffStr.split(',').map(s => parseInt(s.trim()) * 1000);

    let lastError;
    for (let attempt = 0; attempt <= backoffs.length; attempt++) {
        try {
            const result = await fn();
            if (attempt > 0) {
                await setAgentStatus(agentName, 'available');
            }
            return result;
        } catch (err) {
            lastError = err;
            if (attempt < backoffs.length) {
                await setAgentStatus(agentName, 'degraded');
                const delay = backoffs[attempt];
                logVA('retry-backoff', { agent: agentName, attempt: attempt + 1, retries: backoffs.length, delayMs: delay, error: err.message });

                // Notify caller on first failure so they can send feedback to the sender
                if (attempt === 0 && onFirstFailure) {
                    const totalSeconds = Math.ceil(backoffs.reduce((a, b) => a + b, 0) / 1000);
                    try {
                        await onFirstFailure(err, { retriesRemaining: backoffs.length, totalSeconds });
                    } catch (cbErr) {
                        logVA('first-failure-callback-error', { agent: agentName, error: cbErr.message });
                    }
                }

                await new Promise(resolve => setTimeout(resolve, delay));
            }
        }
    }
    // All retries exhausted
    await setAgentStatus(agentName, 'error');
    throw lastError;
}

// --- Error recovery ping ---
// Periodically check errored virtual agents by sending a minimal provider call.

let errorPingTimer = null;

function startErrorPing() {
    if (errorPingTimer) return; // singleton — already started
    const intervalMin = parseInt(config.get('virtual_agent_error_ping_interval')) || 15;
    errorPingTimer = setInterval(pingErroredAgents, intervalMin * 60 * 1000);
    errorPingTimer.unref();
}

async function pingErroredAgents() {
    try {
        const result = await pool.query(
            `SELECT ac.name AS agent
             FROM actors ac
             JOIN agent_configuration agc ON agc.actor_id = ac.id
             WHERE agc.virtual = TRUE AND ac.status IN ('error', 'degraded')`
        );
        for (const row of result.rows) {
            await pingAgent(row.agent);
        }
    } catch (err) {
        logVA('error-ping-scan-failed', { error: err.message });
    }
}

async function pingAgent(agentName) {
    try {
        await invokeAgent(agentName, {
            systemPrompt: 'Respond with OK.',
            userMessage: 'ping',
            context: 'ping',
            skipRateLimit: true,
            skipCostLimit: true,
        });
        await setAgentStatus(agentName, 'available');
        logVA('error-ping-recovered', { agent: agentName });
    } catch (err) {
        logVA('error-ping-still-down', { agent: agentName, error: err.message });
    }
}

// Check if an agent is rate-limited. Returns true if the call should be blocked.
function isRateLimited(agentName) {
    const limit = parseInt(config.get('virtual_agent_rate_limit'));
    const windowMs = parseInt(config.get('virtual_agent_rate_window_seconds')) * 1000;
    const cooldownMs = parseInt(config.get('virtual_agent_cooldown_seconds')) * 1000;

    const now = Date.now();
    if (!callHistory[agentName]) callHistory[agentName] = [];

    const history = callHistory[agentName];

    // Check cooldown: if the most recent call triggered a rate limit, check if cooldown has passed
    if (history._cooldownUntil && now < history._cooldownUntil) {
        logVA('rate-limited', { agent: agentName, reason: 'cooldown', resumesIn: Math.ceil((history._cooldownUntil - now) / 1000) + 's' });
        return true;
    }
    // Clear expired cooldown
    if (history._cooldownUntil && now >= history._cooldownUntil) {
        delete history._cooldownUntil;
    }

    // Prune old entries outside the window
    while (history.length > 0 && history[0] < now - windowMs) {
        history.shift();
    }

    // Check if at limit
    if (history.length >= limit) {
        history._cooldownUntil = now + cooldownMs;
        logVA('rate-limit-triggered', { agent: agentName, calls: history.length, windowSeconds: windowMs / 1000, cooldownSeconds: cooldownMs / 1000 });
        return true;
    }

    return false;
}

// Record a provider call for rate limiting.
function recordCall(agentName) {
    if (!callHistory[agentName]) callHistory[agentName] = [];
    callHistory[agentName].push(Date.now());
}

// Resolve effective cost limits for an agent.
// Agent-specific columns override config defaults. null = unlimited.
function resolveEffectiveLimits(agent) {
    const defaultDailyRaw = config.get('virtual_agent_default_daily_budget');
    const defaultMonthlyRaw = config.get('virtual_agent_default_monthly_budget');

    let dailyLimit = null;
    if (agent.cost_budget_daily != null) {
        dailyLimit = parseFloat(agent.cost_budget_daily);
    } else if (defaultDailyRaw != null) {
        dailyLimit = parseFloat(defaultDailyRaw);
    }

    let monthlyLimit = null;
    if (agent.cost_budget_monthly != null) {
        monthlyLimit = parseFloat(agent.cost_budget_monthly);
    } else if (defaultMonthlyRaw != null) {
        monthlyLimit = parseFloat(defaultMonthlyRaw);
    }

    return { dailyLimit, monthlyLimit };
}

// Check if an agent has exceeded its daily or monthly cost limit.
// Uses rolling windows — no reset logic needed.
// Returns { limited: true, reason: '...' } or { limited: false }.
async function isOverCostLimit(agent) {
    const { dailyLimit, monthlyLimit } = resolveEffectiveLimits(agent);

    // If both limits are null/unset, no enforcement needed
    if (dailyLimit == null && monthlyLimit == null) {
        return { limited: false };
    }

    // Query daily and 30-day rolling cost in a single round-trip
    const result = await pool.query(
        `SELECT
            COALESCE(SUM(CASE WHEN created_at >= DATE_TRUNC('day', NOW() AT TIME ZONE 'UTC') THEN cost ELSE 0 END), 0) AS cost_today,
            COALESCE(SUM(cost), 0) AS cost_monthly
         FROM virtual_agent_usage
         WHERE actor_id = $1 AND created_at >= NOW() - INTERVAL '30 days'`,
        [agent.actor_id]
    );

    const costToday = parseFloat(result.rows[0].cost_today);
    const costMonthly = parseFloat(result.rows[0].cost_monthly);

    if (dailyLimit != null && costToday >= dailyLimit) {
        logVA('over-cost-limit', { agent: agent.agent, costToday, dailyLimit, type: 'daily' });
        return { limited: true, reason: 'Daily cost limit exceeded ($' + costToday.toFixed(4) + ' / $' + dailyLimit.toFixed(2) + ')' };
    }

    if (monthlyLimit != null && costMonthly >= monthlyLimit) {
        logVA('over-cost-limit', { agent: agent.agent, costMonthly, monthlyLimit, type: 'monthly' });
        return { limited: true, reason: 'Monthly cost limit exceeded ($' + costMonthly.toFixed(4) + ' / $' + monthlyLimit.toFixed(2) + ')' };
    }

    return { limited: false };
}

// Record usage and cost after a provider call.
// Inserts a row into virtual_agent_usage with calculated cost.
// Record a usage entry and return its ID (for linking to call detail).
// status defaults to 'success'; pass 'error' + errorMessage for failures.
async function recordUsage(agentName, provider, model, usage, context, status, errorMessage) {
    const cost = calculateCost(provider, model, usage);

    const actor = await requireByName(agentName);
    const result = await pool.query(
        `INSERT INTO virtual_agent_usage (actor_id, provider, model, input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost, context, status, error_message)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
         RETURNING id`,
        [actor.id, provider, model, usage.input_tokens || 0, usage.output_tokens || 0,
         usage.cache_creation_input_tokens || 0, usage.cache_read_input_tokens || 0,
         cost, context || null, status || 'success', errorMessage || null]
    );

    logVA('usage-recorded', { agent: agentName, provider, model, cost: cost.toFixed(6), context,
        input: usage.input_tokens || 0, output: usage.output_tokens || 0, status: status || 'success' });

    return result.rows[0].id;
}

function logVA(action, details) {
    log('virtual-agent', action, details);
}

// Programmatic interface for invoking a virtual agent's LLM.
// Handles: load agent, build provider config, decrypt key, call provider, record usage.
// Options:
//   systemPrompt  — override the agent's startup_instructions (default: use agent's)
//   userMessage    — required, the user/input message
//   context        — usage tracking label (e.g. 'dream', 'soul', 'learning')
//   skipRateLimit  — bypass rate limiter (default: false)
//   skipCostLimit  — bypass cost limit check (default: false)
//   skipRetry      — don't use retryWithBackoff (default: true — callers manage their own error handling)
// Returns: { text, usage, cost } or throws on error.
async function invokeAgent(agentName, options) {
    if (!options || !options.userMessage) {
        throw new Error('invokeAgent: userMessage is required');
    }

    const agent = await loadAgent(agentName);
    if (!agent) {
        throw new Error('Agent not found: ' + agentName);
    }
    if (!agent.api_key || !agent.provider || !agent.model) {
        throw new Error('Agent ' + agentName + ' missing provider/model/api_key');
    }

    // Rate limit check (unless skipped)
    if (!options.skipRateLimit) {
        if (isRateLimited(agentName)) {
            throw new Error('Agent ' + agentName + ' is rate-limited');
        }
    }

    // Cost limit check (unless skipped)
    if (!options.skipCostLimit) {
        const costCheck = await isOverCostLimit(agent);
        if (costCheck.limited) {
            throw new Error('Agent ' + agentName + ' over cost limit: ' + costCheck.reason);
        }
    }

    const apiKey = decryptApiKey(agent.api_key);
    const conf = buildProviderConf(agent);
    const providerFn = createProvider(agent.provider, agent.model, apiKey, conf);

    // Use agent's startup_instructions unless caller provides a system prompt override
    const systemPrompt = options.systemPrompt !== undefined ? options.systemPrompt : (agent.startup_instructions || '');

    // Mark the virtual agent as active while processing
    const actor = await requireByName(agentName);
    await pool.query(
        'UPDATE actors SET active_since = NOW(), last_seen = NOW() WHERE id = $1',
        [actor.id]
    );

    const callFn = async () => {
        return await providerFn(systemPrompt, options.userMessage);
    };

    let result;
    const callStart = Date.now();
    try {
        if (options.skipRetry !== false) {
            result = await callFn();
        } else {
            result = await retryWithBackoff(agentName, callFn);
        }
    } catch (err) {
        // Record the failure in usage and call detail before re-throwing
        const durationMs = Date.now() - callStart;
        const failUsageId = await recordUsage(agentName, agent.provider, agent.model,
            {}, options.context, 'error', err.message).catch(() => null);
        logCall({
            actorId: actor.id, agentName, context: options.context,
            provider: agent.provider, model: agent.model,
            systemPrompt, userMessage: options.userMessage,
            error: err, durationMs, usageId: failUsageId,
        }).catch(() => {});
        throw err;
    } finally {
        // Clear active status when done (keep last_seen updated)
        await pool.query(
            'UPDATE actors SET active_since = NULL, last_seen = NOW() WHERE id = $1',
            [actor.id]
        ).catch(() => {});
    }

    const durationMs = Date.now() - callStart;
    const { text, usage } = result;

    // Record rate limit call
    if (!options.skipRateLimit) {
        recordCall(agentName);
    }

    // Record usage and get its ID for linking
    const cost = calculateCost(agent.provider, agent.model, usage);
    const usageId = await recordUsage(agentName, agent.provider, agent.model, usage, options.context);

    // Log full call details (fire-and-forget)
    logCall({
        actorId: actor.id, agentName, context: options.context,
        provider: agent.provider, model: agent.model,
        systemPrompt, userMessage: options.userMessage,
        response: text, usage, cost, durationMs, usageId,
    }).catch(() => {});

    logVA('invoke', { agent: agentName, context: options.context || null, cost: cost.toFixed(6),
        input: usage.input_tokens || 0, output: usage.output_tokens || 0 });

    return { text, usage, cost };
}

// Check if learning extraction is enabled for an agent.
// Global toggle must be on, and per-agent column can disable.
function isLearningEnabled(agent) {
    const globalEnabled = config.get('virtual_agent_learning_enabled') === 'true';
    if (!globalEnabled) return false;
    if (agent.learning_enabled === false) return false;
    return true;
}

// Build provider configuration from agent data.
// Configuration JSON is the primary source (written by the admin UI).
// Legacy promoted columns (cache_prompts, max_tokens, temperature) are used as
// fallbacks for agents that haven't been edited with the new dynamic config UI.
// Throws if the stored config version doesn't match the current model's version.
function buildProviderConf(agent) {
    let conf = {};
    if (agent.configuration) {
        try { conf = JSON.parse(agent.configuration); } catch (e) { /* ignore */ }
    }

    // Config version check — reject stale configurations
    if (agent.provider && agent.model) {
        const currentVersion = getModelConfigVersion(agent.provider, agent.model);
        if (currentVersion != null) {
            const storedVersion = conf._configVersion || null;
            if (storedVersion == null || storedVersion !== currentVersion) {
                const msg = 'Stale configuration for agent "' + agent.agent + '" — '
                    + 'stored config version ' + (storedVersion || 'none')
                    + ', current version ' + currentVersion
                    + ' for ' + agent.provider + '/' + agent.model
                    + '. Re-save the agent\'s settings in the admin dashboard to update.';
                throw new Error(msg);
            }
        }
    }

    // Legacy column fallbacks — only used if not already set in config JSON
    if (conf.cache_prompts === undefined) {
        conf.cache_prompts = agent.cache_prompts || false;
    }
    if (conf.max_tokens === undefined && agent.max_tokens != null) {
        conf.max_tokens = Number(agent.max_tokens);
    }
    if (conf.temperature === undefined && agent.temperature != null) {
        conf.temperature = Number(agent.temperature);
    }
    return conf;
}

// Build the extraction prompt based on interaction type.
function buildExtractionPrompt(interactionType, contextHint) {
    const base = 'Review the interaction above. Extract 1-3 factual observations worth remembering for future interactions. '
        + 'Focus only on factual information, preferences, or decisions — not tone or style. '
        + 'Include names, identifiers, and concrete details rather than vague generalizations. '
        + 'If nothing new was learned, return exactly NONE.\n\n'
        + 'Format each observation as a bullet point starting with "- ".';

    if (interactionType === 'discussion') {
        return `What factual information, decisions, or preferences did you learn from this discussion about "${contextHint}"?\n\n` + base;
    } else if (interactionType === 'chat') {
        return `What factual information or preferences did you learn about ${contextHint} from this conversation?\n\n` + base;
    } else {
        return `What factual information or action items did you learn from this mail exchange?\n\n` + base;
    }
}

// Log the full interaction transcript as a note in the agent's namespace.
// Fire-and-forget — call with .catch() from the handler.
// Gives reviewable history of what VAs were asked and what they said.
async function logTranscript(agentName, systemPrompt, userMessage, response, usage, triggerType, meta) {
    const now = new Date();
    const pad = (n) => String(n).padStart(2, '0');
    const datePart = `${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}`;
    const timePart = `${pad(now.getHours())}${pad(now.getMinutes())}${pad(now.getSeconds())}`;
    const suffix = Math.random().toString(36).substring(2, 6);
    const slug = `conversations/${datePart}-${timePart}-${suffix}`;

    // Log only the static portion of the system prompt (character instructions,
    // discussion context). The dynamic portion (RAG search results) is ephemeral
    // and must NOT be stored — it gets chunked into the vector store and can
    // cycle back into future RAG queries, creating a feedback loop.
    let systemText;
    if (typeof systemPrompt === 'string') {
        systemText = systemPrompt;
    } else if (systemPrompt && systemPrompt.static) {
        systemText = systemPrompt.static;
    } else {
        const { flattenPrompt } = require('./provider');
        systemText = flattenPrompt(systemPrompt);
    }
    let userText = (typeof userMessage === 'string') ? userMessage : userMessage;

    // Truncate context if too large (keep under 100KB to stay well within 500KB note cap)
    const MAX_CONTEXT = 100000;
    if (systemText.length + userText.length > MAX_CONTEXT) {
        // Keep the user message (most recent/relevant) and truncate system prompt
        const available = MAX_CONTEXT - userText.length;
        if (available > 1000) {
            systemText = systemText.substring(0, available) + '\n\n[... truncated ...]';
        } else {
            systemText = '[truncated — too large]';
            userText = userText.substring(0, MAX_CONTEXT) + '\n\n[... truncated ...]';
        }
    }

    // Build metadata header
    const metaLines = [
        `- **Trigger:** ${triggerType}`,
        meta.requestingAgent ? `- **From:** ${meta.requestingAgent}` : null,
        meta.discussionId ? `- **Discussion:** #${meta.discussionId}` : null,
        meta.model ? `- **Model:** ${meta.model}` : null,
        usage ? `- **Tokens:** ${usage.input_tokens || '?'} in / ${usage.output_tokens || '?'} out` : null,
    ].filter(Boolean).join('\n');

    const content = `## Metadata\n${metaLines}\n\n## System Prompt\n\n${systemText}\n\n## User Message\n\n${userText}\n\n## Response\n\n${response}`;

    const title = `${triggerType} — ${datePart} ${timePart}`;
    await saveNote(agentName, title, content, slug, agentName);
}

// Log a virtual agent call to the virtual_agent_calls table.
// Captures full request/response for diagnostics and fine-tuning.
// Fire-and-forget — call with .catch() from the handler.
// Options:
//   actorId        — actor ID of the virtual agent
//   agentName      — agent name (for logging)
//   context        — trigger type (mail, chat, discussion, dream, soul, learning)
//   contextId      — mail UUID, discussion ID, etc.
//   provider/model — which provider and model were called
//   systemPrompt   — system prompt (string or { static, dynamic } object)
//   userMessage    — user/input message
//   response       — response text (or null on failure)
//   usage          — token usage object from provider (or null on failure)
//   cost           — calculated cost (or 0 on failure)
//   durationMs     — wall-clock time for the call
//   error          — error object (if the call failed)
async function logCall(options) {
    try {
        // Extract the static portion of the system prompt (skip RAG context)
        let systemText;
        if (typeof options.systemPrompt === 'string') {
            systemText = options.systemPrompt;
        } else if (options.systemPrompt && options.systemPrompt.static) {
            systemText = options.systemPrompt.static;
        } else if (options.systemPrompt) {
            const { flattenPrompt } = require('./provider');
            systemText = flattenPrompt(options.systemPrompt);
        } else {
            systemText = null;
        }

        const usage = options.usage || {};
        const status = options.error ? 'error' : 'success';
        const statusCode = options.error ? (options.error.status || options.error.statusCode || null) : null;

        await pool.query(
            `INSERT INTO virtual_agent_calls
             (actor_id, context, context_id, provider, model, system_prompt, user_message,
              response, status, status_code, error_message,
              input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
              cost, duration_ms, usage_id)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
            [
                options.actorId,
                options.context || null,
                options.contextId || null,
                options.provider,
                options.model,
                systemText,
                options.userMessage || null,
                options.error ? (options.error.message || String(options.error)) : (options.response || null),
                status,
                statusCode,
                options.error ? (options.error.message || String(options.error)) : null,
                usage.input_tokens || 0,
                usage.output_tokens || 0,
                usage.cache_read_input_tokens || 0,
                usage.cache_creation_input_tokens || 0,
                options.cost || 0,
                options.durationMs || null,
                options.usageId || null,
            ]
        );
    } catch (err) {
        // Never let call logging break the main flow
        logVA('call-log-error', { agent: options.agentName, error: err.message });
    }
}

// Extract learnings from an interaction and save as a note in the agent's namespace.
// Fire-and-forget — call with .catch() from the handler.
async function extractLearnings(agent, systemPrompt, userMessage, response, interactionType, contextHint) {
    if (!isLearningEnabled(agent)) return;

    // Flatten structured prompt for token estimation and extraction context.
    const { flattenPrompt } = require('./provider');
    const flatPrompt = flattenPrompt(systemPrompt);

    // Check minimum token threshold
    const minTokens = parseInt(config.get('virtual_agent_learning_min_tokens')) || 500;
    const totalChars = (flatPrompt + userMessage + response).length;
    const estimatedTokens = Math.ceil(totalChars / 4);
    if (estimatedTokens < minTokens) {
        logVA('learning-skip-short', { agent: agent.agent, estimatedTokens, minTokens });
        return;
    }

    const extractionPrompt = buildExtractionPrompt(interactionType, contextHint);
    const extractionUserMessage = `System prompt:\n${flatPrompt}\n\nUser message:\n${userMessage}\n\nYour response:\n${response}\n\n---\n\n${extractionPrompt}`;

    const { text: extractionResult } = await invokeAgent(agent.agent, {
        systemPrompt: 'You are a knowledge extraction assistant. Your job is to identify key facts worth remembering from interactions.',
        userMessage: extractionUserMessage,
        context: 'learning',
        skipRateLimit: true,
        skipCostLimit: true,
    });

    // Check for NONE response
    if (extractionResult.trim().toUpperCase() === 'NONE' || extractionResult.trim() === '') {
        logVA('learning-none', { agent: agent.agent, interactionType });
        return;
    }

    // Generate timestamp-based slug
    const now = new Date();
    const datePart = now.toISOString().replace(/[-:T]/g, '').slice(0, 8);
    const timePart = now.toISOString().replace(/[-:T]/g, '').slice(8, 14);
    const slug = `learnings/${datePart}-${timePart}`;
    const title = `Learning ${datePart}-${timePart} (${interactionType})`;

    await saveNote(agent.agent, title, extractionResult, slug, agent.agent);

    logVA('learning-saved', { agent: agent.agent, slug, interactionType, length: extractionResult.length });
}

// Run an async function with the activity spinner on. Ensures the spinner
// stays visible for at least MIN_ACTIVITY_MS even if the call is fast.
async function withActivityIndicator(agentName, fn) {
    const start = Date.now();
    const actor = await requireByName(agentName);
    await pool.query('UPDATE actors SET active_since = NOW(), last_seen = NOW() WHERE id = $1', [actor.id]);
    broadcast('agent_activity', { agent: agentName, active: true });
    try {
        return await fn();
    } finally {
        const remaining = Math.max(0, MIN_ACTIVITY_MS - (Date.now() - start));
        setTimeout(() => {
            pool.query('UPDATE actors SET active_since = NULL, last_seen = NOW() WHERE id = $1', [actor.id]).catch(() => {});
            broadcast('agent_activity', { agent: agentName, active: false });
        }, remaining);
    }
}

// Load an agent row with virtual-agent fields.
async function loadAgent(agentName) {
    const result = await pool.query(
        `SELECT ac.id AS actor_id, ac.name AS agent, agc.virtual, agc.provider, agc.model, agc.api_key, agc.configuration,
                agc.startup_instructions, agc.personality, ac.expertise, agc.cost_budget_daily, agc.cost_budget_monthly,
                agc.cache_prompts, agc.learning_enabled, agc.max_tokens, agc.temperature
         FROM agent_configuration agc
         JOIN actors ac ON ac.id = agc.actor_id
         WHERE ac.name = $1`,
        [agentName]
    );
    if (result.rows.length === 0) return null;
    return result.rows[0];
}

// Get discussion details + participants.
async function loadDiscussion(discussionId) {
    const disc = await pool.query('SELECT * FROM discussions WHERE id = $1', [discussionId]);
    if (disc.rows.length === 0) return null;

    const parts = await pool.query(
        `SELECT ac.name AS agent, dp.status, dp.role
         FROM discussion_participants dp
         JOIN actors ac ON ac.id = dp.actor_id
         WHERE dp.discussion_id = $1`,
        [discussionId]
    );

    return { discussion: disc.rows[0], participants: parts.rows };
}

// Get recent chat history for a discussion, excluding system messages.
async function loadChatHistory(discussionId, limit) {
    const result = await pool.query(
        `SELECT DISTINCT ON (cmt.id) fa.name AS from_agent, cmt.message, cmt.sent_at
         FROM chat_message_texts cmt
         JOIN actors fa ON fa.id = cmt.from_actor_id
         WHERE cmt.discussion_id = $1 AND NOT (fa.name = 'system')
         ORDER BY cmt.id DESC LIMIT $2`,
        [discussionId, limit || 50]
    );
    return result.rows.reverse();
}

// Get recent direct chat history between two agents (no channel/discussion).
// Uses a time window from config (virtual_agent_chat_history_hours) with a count cap
// to keep context relevant without including stale messages from days ago.
async function loadDirectChatHistory(agent1, agent2) {
    const hours = parseInt(config.get('virtual_agent_chat_history_hours')) || 4;
    const maxMessages = 50;

    const actor1 = await requireByName(agent1);
    const actor2 = await requireByName(agent2);

    const result = await pool.query(
        `SELECT fa.name AS from_agent, ta.name AS to_agent, cmt.message, cmt.sent_at
         FROM chat_messages cm
         JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
         JOIN actors fa ON fa.id = cmt.from_actor_id
         JOIN actors ta ON ta.id = cm.to_actor_id
         WHERE cmt.discussion_id IS NULL AND cm.deleted_at IS NULL
         AND ((cmt.from_actor_id = $1 AND cm.to_actor_id = $2) OR (cmt.from_actor_id = $2 AND cm.to_actor_id = $1))
         AND cmt.sent_at >= NOW() - INTERVAL '1 hour' * $3
         ORDER BY cm.id DESC LIMIT $4`,
        [actor1.id, actor2.id, hours, maxMessages]
    );
    return result.rows.reverse();
}

// Search the agent's namespace for context relevant to the discussion topic.
async function loadRAGContext(agentName, query) {
    try {
        const results = await searchMemory(query, agentName, 5);
        if (!results.results || results.results.length === 0) return '';

        return results.results
            .filter(r => r.similarity > 0.3)
            .map(r => `[${r.source_file}] ${r.chunk_text}`)
            .join('\n\n');
    } catch (err) {
        logVA('rag-error', { agent: agentName, error: err.message });
        return '';
    }
}

// Load the context/soul document for a virtual agent, if it exists.
// Returns the soul text or empty string. Never throws.
async function loadSoul(agentName) {
    try {
        const note = await readNote(agentName, 'context/soul');
        return (note && note.content) ? note.content : '';
    } catch (e) {
        return '';
    }
}

// Load per-person relationship files for a virtual agent.
// counterparts is an array of agent names the VA is interacting with.
// Returns formatted string or empty. Never throws.
async function loadPeopleContext(agentName, counterparts) {
    if (!counterparts || counterparts.length === 0) {
        return '';
    }

    const sections = [];
    for (const person of counterparts) {
        try {
            const note = await readNote(agentName, 'context/people/' + person.toLowerCase());
            if (note && note.content) {
                sections.push('## Your impressions of ' + person + '\n\n' + note.content);
            }
        } catch (e) {
            // No relationship file for this person — that's fine
        }
    }

    if (sections.length === 0) {
        return '';
    }

    return sections.join('\n\n');
}

// Build the system prompt for a virtual agent.
// Returns { static, dynamic } — static content is cacheable across calls,
// dynamic content (RAG, closing) changes per message.
function buildSystemPrompt(agent, discussion, ragContext, soul, peopleContext) {
    let staticPart = '';

    // Global bootstrap (prepended to all agents)
    var globalBootstrap = config.get('global_bootstrap') || '';
    if (globalBootstrap) {
        staticPart += globalBootstrap + '\n\n';
    }

    // Agent's own instructions (set via save_instructions)
    if (agent.startup_instructions) {
        staticPart += agent.startup_instructions + '\n\n';
    }

    // Soul document — accumulated identity from dream processing
    if (soul) {
        staticPart += soul + '\n\n';
    }

    // Per-person relationship impressions from dream processing
    if (peopleContext) {
        const preamble = config.get('people_context_preamble') || '';
        if (preamble) {
            staticPart += preamble + '\n\n';
        }
        staticPart += peopleContext + '\n\n';
    }

    // Discussion context — stable for the life of the discussion
    staticPart += `You are "${agent.agent}", a participant in discussion #${discussion.id}.\n`;
    staticPart += `Topic: ${discussion.topic}\n`;
    if (discussion.context) {
        staticPart += `Context: ${discussion.context}\n`;
    }
    staticPart += `Mode: ${discussion.mode}`;

    let dynamicPart = '';

    // RAG context — changes per message
    if (ragContext) {
        dynamicPart += 'Relevant knowledge from your notes:\n' + ragContext + '\n\n';
    }

    dynamicPart += 'Respond concisely and stay on topic. You are participating in a multi-agent discussion.';

    return { static: staticPart, dynamic: dynamicPart };
}

// Build the user message from chat history.
function buildUserMessage(chatHistory, triggerType, voteQuestion) {
    if (chatHistory.length === 0) {
        return 'The discussion has just started. Share your initial thoughts on the topic.';
    }

    let msg = 'Recent discussion:\n\n';
    for (const m of chatHistory) {
        msg += `${m.from_agent}: ${m.message}\n`;
    }

    if (triggerType === 'vote-proposed' && voteQuestion) {
        msg += `\nA vote has been proposed: "${voteQuestion}"\n`;
        msg += 'Reply with ONLY a JSON object: {"choice": 1, "reason": "..."} to approve or {"choice": 2, "reason": "..."} to reject.';
    } else {
        msg += '\nRespond to the discussion.';
    }

    return msg;
}

// Format a timestamp as a compact relative time string.
function formatRelativeTime(sentAt) {
    const now = Date.now();
    const then = new Date(sentAt).getTime();
    const diffMs = now - then;
    const diffSec = Math.floor(diffMs / 1000);
    if (diffSec < 60) return '[now]';
    const diffMin = Math.floor(diffSec / 60);
    if (diffMin < 60) return `[${diffMin}m]`;
    const diffHr = Math.floor(diffMin / 60);
    if (diffHr < 24) return `[${diffHr}h]`;
    const diffDay = Math.floor(diffHr / 24);
    return `[${diffDay}d]`;
}

// Build system prompt for direct chat (no discussion context).
// Returns { static, dynamic } — static content is cacheable across calls.
function buildDirectChatSystemPrompt(agent, ragContext, soul, peopleContext) {
    let staticPart = '';
    var globalBootstrap = config.get('global_bootstrap') || '';
    if (globalBootstrap) {
        staticPart += globalBootstrap + '\n\n';
    }
    if (agent.startup_instructions) {
        staticPart += agent.startup_instructions + '\n\n';
    }
    if (soul) {
        staticPart += soul + '\n\n';
    }
    if (peopleContext) {
        const preamble = config.get('people_context_preamble') || '';
        if (preamble) {
            staticPart += preamble + '\n\n';
        }
        staticPart += peopleContext + '\n\n';
    }
    staticPart += `You are "${agent.agent}". You are chatting directly with another agent.`;
    if (agent.personality) {
        staticPart += `\nYour personality: ${agent.personality}`;
    }

    let dynamicPart = '';
    if (ragContext) {
        dynamicPart += 'Relevant knowledge from your notes:\n' + ragContext + '\n\n';
    }
    dynamicPart += 'Messages include relative timestamps and conversation breaks for context. Respond concisely and naturally.';

    return { static: staticPart, dynamic: dynamicPart };
}

// Build user message for direct chat from conversation history.
// Includes relative timestamps and conversation gap separators.
function buildDirectChatUserMessage(history, fromAgent, latestMessage) {
    if (history.length <= 1) {
        return `${fromAgent}: ${latestMessage}`;
    }

    const GAP_THRESHOLD_MS = 2 * 60 * 60 * 1000; // 2 hours
    let msg = 'Recent conversation:\n\n';

    for (let i = 0; i < history.length; i++) {
        const m = history[i];

        // Insert gap separator if there's a 2+ hour gap between consecutive messages
        if (i > 0 && m.sent_at && history[i - 1].sent_at) {
            const gap = new Date(m.sent_at).getTime() - new Date(history[i - 1].sent_at).getTime();
            if (gap >= GAP_THRESHOLD_MS) {
                const gapHours = Math.round(gap / (60 * 60 * 1000));
                msg += `--- gap: ${gapHours}h ---\n`;
            }
        }

        const timestamp = m.sent_at ? formatRelativeTime(m.sent_at) : '';
        msg += `${timestamp} ${m.from_agent}: ${m.message}\n`;
    }

    msg += '\nRespond to the latest message.';
    return msg;
}

// Build system prompt for mail replies.
// Returns { static, dynamic } for consistency, though mail is one-shot (no caching benefit).
function buildMailSystemPrompt(agent, ragContext, soul, peopleContext) {
    let staticPart = '';
    var globalBootstrap = config.get('global_bootstrap') || '';
    if (globalBootstrap) {
        staticPart += globalBootstrap + '\n\n';
    }
    if (agent.startup_instructions) {
        staticPart += agent.startup_instructions + '\n\n';
    }
    if (soul) {
        staticPart += soul + '\n\n';
    }
    if (peopleContext) {
        const preamble = config.get('people_context_preamble') || '';
        if (preamble) {
            staticPart += preamble + '\n\n';
        }
        staticPart += peopleContext + '\n\n';
    }
    staticPart += `You are "${agent.agent}". You have received a mail message and should compose a reply.`;
    if (agent.personality) {
        staticPart += `\nYour personality: ${agent.personality}`;
    }

    let dynamicPart = '';
    if (ragContext) {
        dynamicPart += 'Relevant knowledge from your notes:\n' + ragContext + '\n\n';
    }
    dynamicPart += 'Compose a thoughtful reply. Write only the reply body — no subject line or headers.';

    return { static: staticPart, dynamic: dynamicPart };
}

// Build user message from an incoming mail.
function buildMailUserMessage(mail) {
    return `From: ${mail.from_agent}\nSubject: ${mail.subject}\n\n${mail.body}`;
}

// Post an error message to the discussion from the virtual agent.
async function postError(agentName, discussionId, error) {
    try {
        await chatSend(agentName, null, discussionId, `[Error: ${error}]`);
    } catch (err) {
        logVA('post-error-failed', { agent: agentName, error: err.message });
    }
}

// Parse a vote response from the LLM. Expects JSON with choice and reason.
function parseVoteResponse(response) {
    // Try to extract JSON from the response
    const jsonMatch = response.match(/\{[^}]+\}/);
    if (!jsonMatch) return null;
    try {
        const parsed = JSON.parse(jsonMatch[0]);
        if (typeof parsed.choice === 'number') {
            return { choice: parsed.choice, reason: parsed.reason || null };
        }
    } catch (e) { /* not valid JSON */ }
    return null;
}

// Cast a vote for a virtual agent.
async function castVote(voteId, agentName, choice, reason) {
    const actor = await requireByName(agentName);

    // Check if already voted
    const existing = await pool.query(
        'SELECT 1 FROM discussion_ballots WHERE vote_id = $1 AND actor_id = $2',
        [voteId, actor.id]
    );
    if (existing.rows.length > 0) {
        logVA('already-voted', { voteId, agent: agentName });
        return;
    }

    await pool.query(
        'INSERT INTO discussion_ballots (vote_id, actor_id, choice, reason) VALUES ($1, $2, $3, $4)',
        [voteId, actor.id, choice, reason]
    );

    // Evaluate vote (may auto-close/auto-conclude)
    const { evaluateVote } = require('./discussion');
    await evaluateVote(voteId);

    logVA('vote-cast', { voteId, agent: agentName, choice });
}

// Main handler — called by system-handler for 'virtual-agent' type messages.
async function handleVirtualAgent(payload) {
    const { discussionId, triggerType, voteId } = payload;

    if (!discussionId) {
        logVA('missing-discussion-id', { payload });
        return;
    }

    const discData = await loadDiscussion(discussionId);
    if (!discData) {
        logVA('discussion-not-found', { discussionId });
        return;
    }

    const { discussion, participants } = discData;

    // Only process active discussions
    if (discussion.status !== 'active') {
        logVA('discussion-not-active', { discussionId, status: discussion.status });
        return;
    }



    // Find virtual participants who are joined
    const joinedVirtual = [];
    for (const p of participants) {
        if (p.status !== 'joined') continue;
        const agent = await loadAgent(p.agent);
        if (agent && agent.virtual) {
            joinedVirtual.push(agent);
        }
    }

    if (joinedVirtual.length === 0) {
        return;
    }

    // Load chat history
    const chatHistory = await loadChatHistory(discussionId, 50);

    // For 'message' triggers, skip if the last non-system message was from a virtual agent
    // (prevents infinite response loops)
    if (triggerType === 'message' && chatHistory.length > 0) {
        const virtualNames = new Set(joinedVirtual.map(a => a.agent));
        const lastNonSystem = [...chatHistory].reverse().find(m => m.from_agent !== 'system');
        if (lastNonSystem && virtualNames.has(lastNonSystem.from_agent)) {
            logVA('skip-self-response', { discussionId, lastFrom: lastNonSystem.from_agent });
            return;
        }
    }

    // Load vote question if this is a vote trigger
    let voteQuestion = null;
    if (triggerType === 'vote-proposed' && voteId) {
        const vote = await pool.query('SELECT question FROM discussion_votes WHERE id = $1', [voteId]);
        if (vote.rows.length > 0) {
            voteQuestion = vote.rows[0].question;
        }
    }

    logVA('processing', { discussionId, triggerType: triggerType || 'message', virtualAgents: joinedVirtual.map(a => a.agent) });

    // Process each virtual agent
    for (const agent of joinedVirtual) {
        try {
            if (!agent.api_key) {
                await postError(agent.agent, discussionId, 'No API key configured');
                continue;
            }
            if (!agent.provider || !agent.model) {
                await postError(agent.agent, discussionId, 'No provider/model configured');
                continue;
            }

            // Decrypt API key
            const apiKey = decryptApiKey(agent.api_key);

            const conf = buildProviderConf(agent);

            // RAG context, soul, and people impressions
            const ragContext = await loadRAGContext(agent.agent, discussion.topic);
            const soul = await loadSoul(agent.agent);
            // Load impressions of all other participants in this discussion
            const counterparts = participants
                .filter(p => p.agent !== agent.agent && p.status === 'joined')
                .map(p => p.agent);
            const peopleContext = await loadPeopleContext(agent.agent, counterparts);

            // Build prompts
            const systemPrompt = buildSystemPrompt(agent, discussion, ragContext, soul, peopleContext);
            const userMessage = buildUserMessage(chatHistory, triggerType, voteQuestion);

            // Rate limit check
            if (isRateLimited(agent.agent)) {
                await postError(agent.agent, discussionId, 'Rate limited — too many API calls. Cooling down.');
                continue;
            }

            // Budget check
            const costCheck = await isOverCostLimit(agent);
            if (costCheck.limited) {
                await postError(agent.agent, discussionId, costCheck.reason);
                continue;
            }

            // Single attempt — no retries for discussions. If the provider fails,
            // the agent is removed from the discussion immediately rather than
            // stalling other participants with a long retry cycle.
            const providerFn = createProvider(agent.provider, agent.model, apiKey, conf);
            recordCall(agent.agent);
            const discussionCallStart = Date.now();
            const { text: response, usage } = await withActivityIndicator(agent.agent, () =>
                providerFn(systemPrompt, userMessage, { cache: true })
            );
            const discussionDurationMs = Date.now() - discussionCallStart;
            const discussionUsageId = await recordUsage(agent.agent, agent.provider, agent.model, usage, 'discussion');

            // Fire-and-forget call logging
            const discussionCost = calculateCost(agent.provider, agent.model, usage);
            logCall({
                actorId: agent.actor_id, agentName: agent.agent, context: 'discussion',
                contextId: String(discussionId), provider: agent.provider, model: agent.model,
                systemPrompt, userMessage, response, usage, cost: discussionCost,
                durationMs: discussionDurationMs, usageId: discussionUsageId,
            }).catch(() => {});

            // Handle vote-proposed: parse response and cast ballot
            if (triggerType === 'vote-proposed' && voteId) {
                const voteResponse = parseVoteResponse(response);
                if (voteResponse) {
                    await castVote(voteId, agent.agent, voteResponse.choice, voteResponse.reason);
                    // Also post the reasoning as a chat message
                    if (voteResponse.reason) {
                        await chatSend(agent.agent, null, discussionId, voteResponse.reason);
                    }
                } else {
                    // Couldn't parse vote, post response as chat and log
                    logVA('vote-parse-failed', { discussionId, agent: agent.agent });
                    await chatSend(agent.agent, null, discussionId, response);
                }
            } else {
                // Regular message response
                await chatSend(agent.agent, null, discussionId, response);
            }

            logVA('responded', { discussionId, agent: agent.agent, triggerType, responseLength: response.length });

            // Fire-and-forget learning extraction (skip for vote responses)
            if (triggerType !== 'vote-proposed') {
                extractLearnings(agent, systemPrompt, userMessage, response, 'discussion', discussion.topic).catch(err => {
                    logVA('learning-extraction-failed', { agent: agent.agent, error: err.message });
                });
            }

            // Fire-and-forget transcript logging
            logTranscript(agent.agent, systemPrompt, userMessage, response, usage, 'discussion', {
                requestingAgent: null, discussionId, model: agent.model
            }).catch(err => {
                logVA('transcript-log-failed', { agent: agent.agent, error: err.message });
            });

        } catch (err) {
            // Provider failed — remove the agent from the discussion so it
            // doesn't block other participants. Error ping will recover it later.
            const discussionErrDuration = typeof discussionCallStart !== 'undefined' ? Date.now() - discussionCallStart : null;
            const discussionFailUsageId = await recordUsage(agent.agent, agent.provider, agent.model,
                {}, 'discussion', 'error', err.message).catch(() => null);
            logCall({
                actorId: agent.actor_id, agentName: agent.agent, context: 'discussion',
                contextId: String(discussionId), provider: agent.provider, model: agent.model,
                systemPrompt, userMessage, error: err, durationMs: discussionErrDuration,
                usageId: discussionFailUsageId,
            }).catch(() => {});
            logError('virtual-agent', 'discussion-agent-error', {
                agent: agent.agent,
                context: 'discussion',
                contextId: String(discussionId),
                message: err.message,
                detail: err.stack,
                statusCode: 500
            });
            await chatSend(agent.agent, null, discussionId,
                `[Malfunction] ${agent.agent} encountered an error and is leaving the discussion: ${err.message}`);
            try {
                const { discussionLeave } = require('./discussion');
                await discussionLeave(discussionId, agent.agent);
            } catch (leaveErr) {
                logVA('discussion-leave-failed', { agent: agent.agent, discussionId, error: leaveErr.message });
            }
            await setAgentStatus(agent.agent, 'error');
        }
    }
}

// Handle a direct chat message sent to a virtual agent (no discussion).
// Called fire-and-forget from chatSend when a non-virtual agent messages a virtual one.
// messageId is the chat_messages.id of the incoming message (for acking after response).
async function handleDirectChat(virtualAgentName, fromAgent, messageText, messageId) {
    const agent = await loadAgent(virtualAgentName);
    if (!agent || !agent.virtual) return;

    if (!agent.api_key || !agent.provider || !agent.model) {
        logVA('direct-chat-skip', { agent: virtualAgentName, reason: 'missing config' });
        return;
    }

    logVA('direct-chat-processing', { agent: virtualAgentName, from: fromAgent });

    try {
        const apiKey = decryptApiKey(agent.api_key);
        const conf = buildProviderConf(agent);

        // Load recent direct chat history between the two agents (time-windowed)
        const history = await loadDirectChatHistory(virtualAgentName, fromAgent);

        // RAG context from the agent's namespace.
        // If the latest message is very short, combine with previous message for better RAG.
        let ragQuery = messageText;
        if (messageText.trim().split(/\s+/).length < 5 && history.length >= 2) {
            const prev = history[history.length - 2];
            ragQuery = prev.message + ' ' + messageText;
        }
        const ragContext = await loadRAGContext(agent.agent, ragQuery);
        const soul = await loadSoul(agent.agent);
        const peopleContext = await loadPeopleContext(agent.agent, [fromAgent]);

        // Build prompts
        const systemPrompt = buildDirectChatSystemPrompt(agent, ragContext, soul, peopleContext);
        const userMessage = buildDirectChatUserMessage(history, fromAgent, messageText);

        // Rate limit check
        if (isRateLimited(agent.agent)) {
            logVA('direct-chat-rate-limited', { agent: virtualAgentName, from: fromAgent });
            await chatSend(virtualAgentName, [fromAgent], null,
                '[Error] Rate limited — too many API calls. Please wait before trying again.', null);
            return;
        }

        // Budget check
        const costCheck = await isOverCostLimit(agent);
        if (costCheck.limited) {
            logVA('direct-chat-over-cost-limit', { agent: virtualAgentName, from: fromAgent, reason: costCheck.reason });
            await chatSend(virtualAgentName, [fromAgent], null,
                `[Error] ${costCheck.reason}`, null);
            return;
        }

        // Call provider with retry+backoff and activity spinner.
        // Pass cache flag — direct chat implies back-and-forth.
        // On first failure, send an immediate chat message so the sender knows retries are in progress.
        const providerFn = createProvider(agent.provider, agent.model, apiKey, conf);
        recordCall(agent.agent);
        const chatCallStart = Date.now();
        const { text: response, usage } = await retryWithBackoff(agent.agent, () =>
            withActivityIndicator(agent.agent, () => providerFn(systemPrompt, userMessage, { cache: true })),
            async (err, retryInfo) => {
                await chatSend(virtualAgentName, [fromAgent], null,
                    `[Retrying] Initial attempt failed: ${err.message}. Retrying ${retryInfo.retriesRemaining} more time(s) over the next ~${formatDuration(retryInfo.totalSeconds)}.`, null);
            }
        );
        const chatDurationMs = Date.now() - chatCallStart;
        const chatUsageId = await recordUsage(agent.agent, agent.provider, agent.model, usage, 'chat');

        // Fire-and-forget call logging
        const chatCost = calculateCost(agent.provider, agent.model, usage);
        logCall({
            actorId: agent.actor_id, agentName: agent.agent, context: 'chat',
            provider: agent.provider, model: agent.model,
            systemPrompt, userMessage, response, usage, cost: chatCost,
            durationMs: chatDurationMs, usageId: chatUsageId,
        }).catch(() => {});

        // Send response as direct chat back to the sender
        await chatSend(agent.agent, [fromAgent], null, response);

        // Ack the incoming message (virtual agent "read" it)
        if (messageId) {
            await pool.query(
                'UPDATE chat_messages SET acked_at = NOW() WHERE id = $1 AND to_actor_id = $2 AND acked_at IS NULL',
                [messageId, agent.actor_id]
            );
        }

        logVA('direct-chat-responded', { agent: agent.agent, to: fromAgent, responseLength: response.length });

        // Fire-and-forget learning extraction
        extractLearnings(agent, systemPrompt, userMessage, response, 'chat', fromAgent).catch(err => {
            logVA('learning-extraction-failed', { agent: agent.agent, error: err.message });
        });

        // Fire-and-forget transcript logging
        logTranscript(agent.agent, systemPrompt, userMessage, response, usage, 'chat', {
            requestingAgent: fromAgent, model: agent.model
        }).catch(err => {
            logVA('transcript-log-failed', { agent: agent.agent, error: err.message });
        });
    } catch (err) {
        // Log the failed call
        const chatErrDuration = typeof chatCallStart !== 'undefined' ? Date.now() - chatCallStart : null;
        const chatFailUsageId = agent ? await recordUsage(agent.agent, agent.provider, agent.model,
            {}, 'chat', 'error', err.message).catch(() => null) : null;
        logCall({
            actorId: agent ? agent.actor_id : null, agentName: virtualAgentName, context: 'chat',
            provider: agent ? agent.provider : 'unknown', model: agent ? agent.model : 'unknown',
            systemPrompt: typeof systemPrompt !== 'undefined' ? systemPrompt : null,
            userMessage: typeof userMessage !== 'undefined' ? userMessage : messageText,
            error: err, durationMs: chatErrDuration, usageId: chatFailUsageId,
        }).catch(() => {});
        logError('virtual-agent', 'direct-chat-error', {
            agent: virtualAgentName,
            context: 'chat',
            message: err.message,
            detail: err.stack,
            statusCode: 500
        });
        // Send error feedback to the caller so they know it failed
        try {
            await chatSend(virtualAgentName, [fromAgent], null,
                `[Error] ${virtualAgentName} is unavailable (${err.message}).`, null);
        } catch (sendErr) {
            logVA('error-feedback-failed', { agent: virtualAgentName, error: sendErr.message });
        }
    }
}

// Handle a mail sent to a virtual agent.
// Called fire-and-forget from mailSend when a non-virtual agent mails a virtual one.
async function handleDirectMail(virtualAgentName, fromAgent, mailId) {
    logVA('direct-mail-entry', { agent: virtualAgentName, from: fromAgent, mailId });
    const agent = await loadAgent(virtualAgentName);
    if (!agent || !agent.virtual) return;

    if (!agent.api_key || !agent.provider || !agent.model) {
        logVA('direct-mail-skip', { agent: virtualAgentName, reason: 'missing config' });
        return;
    }

    // Load the incoming mail — JOIN with actors to get from/to names
    const mailResult = await pool.query(
        `SELECT m.*, fa.name AS from_agent, ta.name AS to_agent
         FROM mail m
         JOIN actors fa ON fa.id = m.from_actor_id
         JOIN actors ta ON ta.id = m.to_actor_id
         WHERE m.id = $1`,
        [mailId]
    );
    if (mailResult.rows.length === 0) return;
    const mail = mailResult.rows[0];

    logVA('direct-mail-processing', { agent: virtualAgentName, from: fromAgent, mailId, subject: mail.subject });

    try {
        const apiKey = decryptApiKey(agent.api_key);
        const conf = buildProviderConf(agent);

        // RAG context from the agent's namespace
        const ragContext = await loadRAGContext(agent.agent, `${mail.subject} ${mail.body}`);
        const soul = await loadSoul(agent.agent);
        const peopleContext = await loadPeopleContext(agent.agent, [fromAgent]);

        // Build prompts
        const systemPrompt = buildMailSystemPrompt(agent, ragContext, soul, peopleContext);
        const userMessage = buildMailUserMessage(mail);

        // Rate limit check
        if (isRateLimited(agent.agent)) {
            logVA('direct-mail-rate-limited', { agent: virtualAgentName, from: fromAgent, mailId });
            const { mailSend: mailSendErr } = require('./mail');
            const errSubject = mail.subject.startsWith('Re: ') ? mail.subject : `Re: ${mail.subject}`;
            await mailSendErr(fromAgent, virtualAgentName, errSubject,
                '[Error] Rate limited — too many API calls. Please wait before trying again.', mailId);
            return;
        }

        // Budget check
        const costCheck = await isOverCostLimit(agent);
        if (costCheck.limited) {
            logVA('direct-mail-over-cost-limit', { agent: virtualAgentName, from: fromAgent, mailId, reason: costCheck.reason });
            const { mailSend: mailSendErr } = require('./mail');
            const errSubject = mail.subject.startsWith('Re: ') ? mail.subject : `Re: ${mail.subject}`;
            await mailSendErr(fromAgent, virtualAgentName, errSubject,
                `[Error] ${costCheck.reason}`, mailId);
            return;
        }

        // Call provider with retry+backoff and activity spinner.
        // On first failure, send an immediate ack mail so the sender knows retries are in progress.
        const providerFn = createProvider(agent.provider, agent.model, apiKey, conf);
        recordCall(agent.agent);
        const replySubjectPrefix = mail.subject.startsWith('Re: ') ? mail.subject : `Re: ${mail.subject}`;
        const mailCallStart = Date.now();
        const { text: response, usage } = await retryWithBackoff(agent.agent, () =>
            withActivityIndicator(agent.agent, () => providerFn(systemPrompt, userMessage)),
            async (err, retryInfo) => {
                const { mailSend: mailSendAck } = require('./mail');
                await mailSendAck(fromAgent, virtualAgentName, replySubjectPrefix,
                    `[Retrying] Initial attempt failed: ${err.message}\n\nRetrying ${retryInfo.retriesRemaining} more time(s) over the next ~${formatDuration(retryInfo.totalSeconds)}. You'll receive the final response or a failure notice.`,
                    mailId);
            }
        );
        const mailDurationMs = Date.now() - mailCallStart;
        const mailUsageId = await recordUsage(agent.agent, agent.provider, agent.model, usage, 'mail');

        // Fire-and-forget call logging
        const mailCost = calculateCost(agent.provider, agent.model, usage);
        logCall({
            actorId: agent.actor_id, agentName: agent.agent, context: 'mail',
            contextId: mailId, provider: agent.provider, model: agent.model,
            systemPrompt, userMessage, response, usage, cost: mailCost,
            durationMs: mailDurationMs, usageId: mailUsageId,
        }).catch(() => {});

        // Ack the incoming mail (virtual agent "read" it)
        await pool.query(
            'UPDATE mail SET acked_at = NOW() WHERE id = $1 AND to_actor_id = $2 AND acked_at IS NULL',
            [mailId, agent.actor_id]
        );

        // Send reply mail (threaded via in_reply_to)
        const { mailSend } = require('./mail');
        await mailSend(fromAgent, agent.agent, replySubjectPrefix, response, mailId);

        logVA('direct-mail-responded', { agent: agent.agent, to: fromAgent, mailId, responseLength: response.length });

        // Fire-and-forget learning extraction
        extractLearnings(agent, systemPrompt, userMessage, response, 'mail', null).catch(err => {
            logVA('learning-extraction-failed', { agent: agent.agent, error: err.message });
        });

        // Fire-and-forget transcript logging
        logTranscript(agent.agent, systemPrompt, userMessage, response, usage, 'mail', {
            requestingAgent: fromAgent, model: agent.model
        }).catch(err => {
            logVA('transcript-log-failed', { agent: agent.agent, error: err.message });
        });
    } catch (err) {
        // Log the failed call
        const mailErrDuration = typeof mailCallStart !== 'undefined' ? Date.now() - mailCallStart : null;
        const mailFailUsageId = agent ? await recordUsage(agent.agent, agent.provider, agent.model,
            {}, 'mail', 'error', err.message).catch(() => null) : null;
        logCall({
            actorId: agent ? agent.actor_id : null, agentName: virtualAgentName, context: 'mail',
            contextId: mailId, provider: agent ? agent.provider : 'unknown', model: agent ? agent.model : 'unknown',
            systemPrompt: typeof systemPrompt !== 'undefined' ? systemPrompt : null,
            userMessage: typeof userMessage !== 'undefined' ? userMessage : null,
            error: err, durationMs: mailErrDuration, usageId: mailFailUsageId,
        }).catch(() => {});
        logError('virtual-agent', 'direct-mail-error', {
            agent: virtualAgentName,
            context: 'mail',
            contextId: mailId,
            message: err.message,
            detail: err.stack,
            statusCode: 500
        });
        // Ack the incoming mail so it doesn't stay stuck
        await pool.query(
            'UPDATE mail SET acked_at = NOW() WHERE id = $1 AND to_actor_id = $2 AND acked_at IS NULL',
            [mailId, agent.actor_id]
        ).catch(() => {});
        // Send error reply threaded to the original message
        try {
            const { mailSend: mailSendErr } = require('./mail');
            await mailSendErr(fromAgent, virtualAgentName,
                'Delivery failed',
                `${virtualAgentName} is unavailable (${err.message}). Your message has been acked — resend when the agent is back online.`,
                mailId);
        } catch (sendErr) {
            logVA('error-feedback-failed', { agent: virtualAgentName, error: sendErr.message });
        }
    }
}

// Register with system handler on load.
const systemHandler = require('./system-handler');
systemHandler.register('virtual-agent', handleVirtualAgent);

module.exports = { handleVirtualAgent, handleDirectChat, handleDirectMail, resolveEffectiveLimits, startErrorPing, invokeAgent, loadAgent };
