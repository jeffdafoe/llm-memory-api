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
const { saveNote } = require('./documents');
const config = require('./config');
const { requireByName } = require('./actors');

const MIN_ACTIVITY_MS = 3000;

// In-memory rate limiter: agent -> array of call timestamps
const callHistory = {};

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
async function recordUsage(agentName, provider, model, usage, context) {
    const cost = calculateCost(provider, model, usage);

    const actor = await requireByName(agentName);
    await pool.query(
        `INSERT INTO virtual_agent_usage (actor_id, provider, model, input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost, context)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
        [actor.id, provider, model, usage.input_tokens || 0, usage.output_tokens || 0,
         usage.cache_creation_input_tokens || 0, usage.cache_read_input_tokens || 0,
         cost, context || null]
    );

    logVA('usage-recorded', { agent: agentName, provider, model, cost: cost.toFixed(6), context,
        input: usage.input_tokens || 0, output: usage.output_tokens || 0 });
}

function logVA(action, details) {
    log('virtual-agent', action, details);
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
        conf.max_tokens = agent.max_tokens;
    }
    if (conf.temperature === undefined && agent.temperature != null) {
        conf.temperature = agent.temperature;
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

// Extract learnings from an interaction and save as a note in the agent's namespace.
// Fire-and-forget — call with .catch() from the handler.
async function extractLearnings(agent, systemPrompt, userMessage, response, interactionType, contextHint, provider) {
    if (!isLearningEnabled(agent)) return;

    // Flatten structured prompt for token estimation and extraction context.
    // Extraction is a one-shot call — no caching benefit.
    const { flattenPrompt } = require('./provider');
    const flatPrompt = flattenPrompt(systemPrompt);

    // Check minimum token threshold
    const minTokens = parseInt(config.get('virtual_agent_learning_min_tokens')) || 500;
    // We don't have exact token counts here, but we can estimate from the usage
    // that was already recorded. Instead, use a character-based heuristic:
    // average ~4 chars per token, so check total chars of input+output.
    const totalChars = (flatPrompt + userMessage + response).length;
    const estimatedTokens = Math.ceil(totalChars / 4);
    if (estimatedTokens < minTokens) {
        logVA('learning-skip-short', { agent: agent.agent, estimatedTokens, minTokens });
        return;
    }

    const extractionPrompt = buildExtractionPrompt(interactionType, contextHint);

    // Build the extraction user message: the full interaction context + extraction prompt
    const extractionUserMessage = `System prompt:\n${flatPrompt}\n\nUser message:\n${userMessage}\n\nYour response:\n${response}\n\n---\n\n${extractionPrompt}`;
    const extractionSystemPrompt = 'You are a knowledge extraction assistant. Your job is to identify key facts worth remembering from interactions.';

    const { text: extractionResult, usage } = await provider(extractionSystemPrompt, extractionUserMessage);
    await recordUsage(agent.agent, agent.provider, agent.model, usage, 'learning');

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

// Get recent chat history for a discussion channel, excluding system-to-system triggers.
async function loadChatHistory(channel, limit) {
    const result = await pool.query(
        `SELECT fa.name AS from_agent, ta.name AS to_agent, cm.message, cm.sent_at
         FROM chat_messages cm
         JOIN actors fa ON fa.id = cm.from_actor_id
         JOIN actors ta ON ta.id = cm.to_actor_id
         WHERE cm.channel = $1 AND cm.deleted_at IS NULL AND NOT (fa.name = 'system' AND ta.name = 'system')
         ORDER BY cm.id DESC LIMIT $2`,
        [channel, limit || 50]
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
        `SELECT fa.name AS from_agent, ta.name AS to_agent, cm.message, cm.sent_at
         FROM chat_messages cm
         JOIN actors fa ON fa.id = cm.from_actor_id
         JOIN actors ta ON ta.id = cm.to_actor_id
         WHERE cm.channel IS NULL AND cm.deleted_at IS NULL
         AND ((cm.from_actor_id = $1 AND cm.to_actor_id = $2) OR (cm.from_actor_id = $2 AND cm.to_actor_id = $1))
         AND cm.sent_at >= NOW() - INTERVAL '1 hour' * $3
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

// Build the system prompt for a virtual agent.
// Returns { static, dynamic } — static content is cacheable across calls,
// dynamic content (RAG, closing) changes per message.
function buildSystemPrompt(agent, discussion, ragContext) {
    let staticPart = '';

    // Agent's own instructions (set via save_instructions)
    if (agent.startup_instructions) {
        staticPart += agent.startup_instructions + '\n\n';
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
function buildDirectChatSystemPrompt(agent, ragContext) {
    let staticPart = '';
    if (agent.startup_instructions) {
        staticPart += agent.startup_instructions + '\n\n';
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
function buildMailSystemPrompt(agent, ragContext) {
    let staticPart = '';
    if (agent.startup_instructions) {
        staticPart += agent.startup_instructions + '\n\n';
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

// Post an error message to the discussion channel from the virtual agent.
async function postError(agentName, discussionId, channel, error) {
    try {
        await chatSend(agentName, null, discussionId, `[Error: ${error}]`, channel);
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

    const channel = discussion.channel || `discussion-${discussionId}`;

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
    const chatHistory = await loadChatHistory(channel, 50);

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
                await postError(agent.agent, discussionId, channel, 'No API key configured');
                continue;
            }
            if (!agent.provider || !agent.model) {
                await postError(agent.agent, discussionId, channel, 'No provider/model configured');
                continue;
            }

            // Decrypt API key
            const apiKey = decryptApiKey(agent.api_key);

            const conf = buildProviderConf(agent);

            // RAG context
            const ragContext = await loadRAGContext(agent.agent, discussion.topic);

            // Build prompts
            const systemPrompt = buildSystemPrompt(agent, discussion, ragContext);
            const userMessage = buildUserMessage(chatHistory, triggerType, voteQuestion);

            // Rate limit check
            if (isRateLimited(agent.agent)) {
                await postError(agent.agent, discussionId, channel, 'Rate limited — too many API calls. Cooling down.');
                continue;
            }

            // Budget check
            const costCheck = await isOverCostLimit(agent);
            if (costCheck.limited) {
                await postError(agent.agent, discussionId, channel, costCheck.reason);
                continue;
            }

            // Call provider with activity spinner (minimum 3s visibility).
            // Pass cache flag for chat-based interactions (discussions have repeated calls).
            const provider = createProvider(agent.provider, agent.model, apiKey, conf);
            recordCall(agent.agent);
            const { text: response, usage } = await withActivityIndicator(agent.agent, () => provider(systemPrompt, userMessage, { cache: true }));
            await recordUsage(agent.agent, agent.provider, agent.model, usage, 'discussion');

            // Handle vote-proposed: parse response and cast ballot
            if (triggerType === 'vote-proposed' && voteId) {
                const voteResponse = parseVoteResponse(response);
                if (voteResponse) {
                    await castVote(voteId, agent.agent, voteResponse.choice, voteResponse.reason);
                    // Also post the reasoning as a chat message
                    if (voteResponse.reason) {
                        await chatSend(agent.agent, null, discussionId, voteResponse.reason, channel);
                    }
                } else {
                    // Couldn't parse vote, post response as chat and log
                    logVA('vote-parse-failed', { discussionId, agent: agent.agent });
                    await chatSend(agent.agent, null, discussionId, response, channel);
                }
            } else {
                // Regular message response
                await chatSend(agent.agent, null, discussionId, response, channel);
            }

            logVA('responded', { discussionId, agent: agent.agent, triggerType, responseLength: response.length });

            // Fire-and-forget learning extraction (skip for vote responses)
            if (triggerType !== 'vote-proposed') {
                extractLearnings(agent, systemPrompt, userMessage, response, 'discussion', discussion.topic, provider).catch(err => {
                    logVA('learning-extraction-failed', { agent: agent.agent, error: err.message });
                });
            }

        } catch (err) {
            logError('virtual-agent', 'discussion-agent-error', {
                agent: agent.agent,
                context: 'discussion',
                contextId: String(discussionId),
                message: err.message,
                detail: err.stack
            });
            await postError(agent.agent, discussionId, channel, err.message);
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

        // Build prompts
        const systemPrompt = buildDirectChatSystemPrompt(agent, ragContext);
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

        // Call provider with activity spinner (minimum 3s visibility).
        // Pass cache flag — direct chat implies back-and-forth.
        const provider = createProvider(agent.provider, agent.model, apiKey, conf);
        recordCall(agent.agent);
        const { text: response, usage } = await withActivityIndicator(agent.agent, () => provider(systemPrompt, userMessage, { cache: true }));
        await recordUsage(agent.agent, agent.provider, agent.model, usage, 'chat');

        // Send response as direct chat back to the sender
        await chatSend(agent.agent, [fromAgent], null, response, null);

        // Ack the incoming message (virtual agent "read" it)
        if (messageId) {
            await pool.query(
                'UPDATE chat_messages SET acked_at = NOW() WHERE id = $1 AND to_actor_id = $2 AND acked_at IS NULL',
                [messageId, agent.actor_id]
            );
        }

        logVA('direct-chat-responded', { agent: agent.agent, to: fromAgent, responseLength: response.length });

        // Fire-and-forget learning extraction
        extractLearnings(agent, systemPrompt, userMessage, response, 'chat', fromAgent, provider).catch(err => {
            logVA('learning-extraction-failed', { agent: agent.agent, error: err.message });
        });
    } catch (err) {
        logError('virtual-agent', 'direct-chat-error', {
            agent: virtualAgentName,
            context: 'chat',
            message: err.message,
            detail: err.stack
        });
        // Send error feedback to the caller so they know it failed
        try {
            await chatSend(virtualAgentName, [fromAgent], null,
                `[Error] ${err.message}`, null);
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

        // Build prompts
        const systemPrompt = buildMailSystemPrompt(agent, ragContext);
        const userMessage = buildMailUserMessage(mail);

        // Rate limit check
        if (isRateLimited(agent.agent)) {
            logVA('direct-mail-rate-limited', { agent: virtualAgentName, from: fromAgent, mailId });
            const { mailSend: mailSendErr } = require('./mail');
            const errSubject = mail.subject.startsWith('Re: ') ? mail.subject : `Re: ${mail.subject}`;
            await mailSendErr(fromAgent, virtualAgentName, errSubject,
                '[Error] Rate limited — too many API calls. Please wait before trying again.');
            return;
        }

        // Budget check
        const costCheck = await isOverCostLimit(agent);
        if (costCheck.limited) {
            logVA('direct-mail-over-cost-limit', { agent: virtualAgentName, from: fromAgent, mailId, reason: costCheck.reason });
            const { mailSend: mailSendErr } = require('./mail');
            const errSubject = mail.subject.startsWith('Re: ') ? mail.subject : `Re: ${mail.subject}`;
            await mailSendErr(fromAgent, virtualAgentName, errSubject,
                `[Error] ${costCheck.reason}`);
            return;
        }

        // Call provider with activity spinner (minimum 3s visibility)
        const provider = createProvider(agent.provider, agent.model, apiKey, conf);
        recordCall(agent.agent);
        const { text: response, usage } = await withActivityIndicator(agent.agent, () => provider(systemPrompt, userMessage));
        await recordUsage(agent.agent, agent.provider, agent.model, usage, 'mail');

        // Ack the incoming mail (virtual agent "read" it)
        await pool.query(
            'UPDATE mail SET acked_at = NOW() WHERE id = $1 AND to_actor_id = $2 AND acked_at IS NULL',
            [mailId, agent.actor_id]
        );

        // Send reply mail
        const replySubject = mail.subject.startsWith('Re: ') ? mail.subject : `Re: ${mail.subject}`;
        const { mailSend } = require('./mail');
        await mailSend(fromAgent, agent.agent, replySubject, response);

        logVA('direct-mail-responded', { agent: agent.agent, to: fromAgent, mailId, responseLength: response.length });

        // Fire-and-forget learning extraction
        extractLearnings(agent, systemPrompt, userMessage, response, 'mail', null, provider).catch(err => {
            logVA('learning-extraction-failed', { agent: agent.agent, error: err.message });
        });
    } catch (err) {
        logError('virtual-agent', 'direct-mail-error', {
            agent: virtualAgentName,
            context: 'mail',
            contextId: mailId,
            message: err.message,
            detail: err.stack
        });
        // Send error reply mail so the caller knows it failed
        try {
            const { mailSend: mailSendErr } = require('./mail');
            const mailResult = await pool.query('SELECT subject FROM mail WHERE id = $1', [mailId]);
            const subject = mailResult.rows.length > 0 ? mailResult.rows[0].subject : 'Unknown';
            const errSubject = subject.startsWith('Re: ') ? subject : `Re: ${subject}`;
            await mailSendErr(fromAgent, virtualAgentName, errSubject,
                `[Error] ${err.message}`);
        } catch (sendErr) {
            logVA('error-feedback-failed', { agent: virtualAgentName, error: sendErr.message });
        }
    }
}

// Register with system handler on load.
const systemHandler = require('./system-handler');
systemHandler.register('virtual-agent', handleVirtualAgent);

module.exports = { handleVirtualAgent, handleDirectChat, handleDirectMail, resolveEffectiveLimits };
