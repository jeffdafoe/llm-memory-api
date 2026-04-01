// Dream processing — nightly conversation log analysis.
// Reads conversation logs uploaded by agents, sends them through a dream
// virtual agent (companion or technical), and saves consolidated insights
// as notes in the agent's namespace.

const pool = require('../db');
const config = require('./config');
const { log } = require('./logger');
const { createProvider, decryptApiKey } = require('./provider');
const { saveNote } = require('./documents');
const { requireByName } = require('./actors');

// Signal patterns that indicate memory-worthy content.
// Used to pre-filter conversation logs before sending to the dream agent,
// keeping only passages around these signals + surrounding context.
const SIGNAL_PATTERNS = [
    // Explicit memory requests
    /\bremember\b/i,
    /\bdon'?t forget\b/i,
    /\bnote that\b/i,
    /\bkeep in mind\b/i,
    // Corrections and feedback
    /\bdon'?t do that\b/i,
    /\bstop doing\b/i,
    /\bnot like that\b/i,
    /\bwrong\b/i,
    /\binstead\b/i,
    /\bactually\b/i,
    /\bno,?\s/i,
    // Preferences and decisions
    /\bfrom now on\b/i,
    /\balways\b/i,
    /\bnever\b/i,
    /\bprefer\b/i,
    /\bI like\b/i,
    /\bI hate\b/i,
    /\bI want\b/i,
    // Temporal / deadline signals
    /\bdeadline\b/i,
    /\bby\s+(monday|tuesday|wednesday|thursday|friday|saturday|sunday|tomorrow|next week)\b/i,
    /\bdue\s+(date|by)\b/i,
    // Reasons and context
    /\bthe reason\b/i,
    /\bbecause\b/i,
    /\bimportant\b/i,
    // Emotional signals (for companion mode)
    /\bfeeling\b/i,
    /\bworried\b/i,
    /\bexcited\b/i,
    /\bfrustrat/i,
    /\bhappy\b/i,
    /\bsad\b/i,
    /\bstress/i,
    /\banxious/i,
    /\bthank you\b/i,
    /\blove\b/i,
    /\bmiss\b/i,
];

// How many context lines to include before and after a signal match
const CONTEXT_LINES = 5;

function logDream(action, details) {
    log('dream', action, details);
}

// Pre-filter a conversation log to only signal-bearing passages.
// Returns a reduced version of the log with signal lines + surrounding context.
function prefilterLog(content) {
    const lines = content.split('\n');

    // Find which lines contain signals
    const signalLineIndices = new Set();
    for (let i = 0; i < lines.length; i++) {
        for (const pattern of SIGNAL_PATTERNS) {
            if (pattern.test(lines[i])) {
                signalLineIndices.add(i);
                break;
            }
        }
    }

    if (signalLineIndices.size === 0) {
        return null; // No signals found — nothing worth dreaming about
    }

    // Expand to include context around each signal
    const includedLines = new Set();
    for (const idx of signalLineIndices) {
        const start = Math.max(0, idx - CONTEXT_LINES);
        const end = Math.min(lines.length - 1, idx + CONTEXT_LINES);
        for (let i = start; i <= end; i++) {
            includedLines.add(i);
        }
    }

    // Build the filtered content, inserting separators where lines are skipped
    const result = [];
    let lastIncluded = -2;
    for (let i = 0; i < lines.length; i++) {
        if (includedLines.has(i)) {
            if (i > lastIncluded + 1 && lastIncluded >= 0) {
                result.push('  [...]');
            }
            result.push(lines[i]);
            lastIncluded = i;
        }
    }

    return result.join('\n');
}

// Load the dream virtual agent and verify it's system-owned.
// Returns the agent data or null with an error logged.
async function loadDreamAgent(modeName) {
    const agentName = 'dream-' + modeName;

    const result = await pool.query(
        `SELECT ac.id AS actor_id, ac.name AS agent, ac.created_by,
                agc.provider, agc.model, agc.api_key, agc.configuration,
                agc.startup_instructions, agc.personality, agc.virtual,
                agc.cache_prompts, agc.learning_enabled, agc.max_tokens, agc.temperature
         FROM agent_configuration agc
         JOIN actors ac ON ac.id = agc.actor_id
         WHERE ac.name = $1`,
        [agentName]
    );

    if (result.rows.length === 0) {
        logDream('error', { message: 'Dream agent not found: ' + agentName });
        return null;
    }

    const agent = result.rows[0];

    // Verify system ownership
    const systemActor = await pool.query("SELECT id FROM actors WHERE name = 'system'");
    if (systemActor.rows.length === 0) {
        logDream('error', { message: 'System actor not found' });
        return null;
    }
    if (agent.created_by !== systemActor.rows[0].id) {
        logDream('error', { message: agentName + ' is not owned by system (created_by=' + agent.created_by + ')' });
        return null;
    }

    if (!agent.api_key || !agent.provider || !agent.model) {
        logDream('error', { message: agentName + ' missing provider/model/api_key' });
        return null;
    }

    return agent;
}

// Build provider configuration from agent data (same logic as virtual-agent.js)
function buildConf(agent) {
    let conf = {};
    if (agent.configuration) {
        try { conf = JSON.parse(agent.configuration); } catch (e) { /* ignore */ }
    }
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

// Record cost usage for the dream agent
async function recordDreamUsage(agentName, provider, model, usage) {
    const { calculateCost } = require('./providers');
    const cost = calculateCost(provider, model, usage);
    const actor = await requireByName(agentName);

    await pool.query(
        `INSERT INTO virtual_agent_usage (actor_id, provider, model, input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost, context)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
        [actor.id, provider, model, usage.input_tokens || 0, usage.output_tokens || 0,
         usage.cache_creation_input_tokens || 0, usage.cache_read_input_tokens || 0,
         cost, 'dream']
    );

    logDream('usage', { agent: agentName, cost: cost.toFixed(6), input: usage.input_tokens || 0, output: usage.output_tokens || 0 });
}

// Slugify a title for note storage
function slugify(text) {
    return text.toLowerCase()
        .replace(/[^a-z0-9]+/g, '-')
        .replace(/^-+|-+$/g, '')
        .slice(0, 60);
}

// Run the dream processing job.
// Returns a summary object with counts and any errors.
async function runDream() {
    // Check global switch
    if (config.get('dream_processing_enabled') !== 'true') {
        logDream('skip', { reason: 'dream_processing_enabled is false' });
        return { skipped: true, reason: 'disabled' };
    }

    // Load both dream agents
    const companionAgent = await loadDreamAgent('companion');
    const technicalAgent = await loadDreamAgent('technical');

    if (!companionAgent && !technicalAgent) {
        logDream('abort', { reason: 'Neither dream agent found or valid' });
        return { error: 'No valid dream agents found. Both dream-companion and dream-technical must exist and be owned by system.' };
    }

    // Find agents with dream mode enabled
    const agents = await pool.query(
        `SELECT ac.name, ac.id AS actor_id, agc.dream_mode, agc.last_dream_at
         FROM agent_configuration agc
         JOIN actors ac ON ac.id = agc.actor_id
         WHERE agc.dream_mode IN ('companion', 'technical')
         AND agc.virtual = false`
    );

    if (agents.rows.length === 0) {
        logDream('skip', { reason: 'No agents with dream mode enabled' });
        return { processed: 0, reason: 'No agents with dream mode enabled' };
    }

    logDream('start', { agents: agents.rows.map(a => a.name + ':' + a.dream_mode) });

    const results = [];

    for (const agent of agents.rows) {
        try {
            // Pick the right dream agent
            const dreamAgent = agent.dream_mode === 'companion' ? companionAgent : technicalAgent;
            if (!dreamAgent) {
                results.push({ agent: agent.name, error: 'dream-' + agent.dream_mode + ' agent not available' });
                continue;
            }

            // Get conversation logs since last dream (or last 24h if never run)
            const since = agent.last_dream_at || new Date(Date.now() - 24 * 60 * 60 * 1000).toISOString();

            const logs = await pool.query(
                `SELECT slug, content, created_at FROM documents
                 WHERE namespace = $1 AND slug LIKE 'conversations/%' AND deleted_at IS NULL
                 AND created_at > $2
                 ORDER BY created_at ASC`,
                [agent.name, since]
            );

            if (logs.rows.length === 0) {
                logDream('no-logs', { agent: agent.name, since });
                results.push({ agent: agent.name, skipped: true, reason: 'No new conversation logs' });
                // Still update last_dream_at so we don't re-scan the same window
                await pool.query(
                    'UPDATE agent_configuration SET last_dream_at = NOW() WHERE actor_id = $1',
                    [agent.actor_id]
                );
                continue;
            }

            // Concatenate all conversation logs for the period
            let fullLog = logs.rows.map(r => r.content).join('\n\n---\n\n');

            // Pre-filter to signal-bearing passages
            const filtered = prefilterLog(fullLog);
            if (!filtered) {
                logDream('no-signals', { agent: agent.name, logCount: logs.rows.length });
                results.push({ agent: agent.name, skipped: true, reason: 'No signal-bearing content found' });
                await pool.query(
                    'UPDATE agent_configuration SET last_dream_at = NOW() WHERE actor_id = $1',
                    [agent.actor_id]
                );
                continue;
            }

            logDream('processing', {
                agent: agent.name,
                mode: agent.dream_mode,
                logCount: logs.rows.length,
                originalSize: fullLog.length,
                filteredSize: filtered.length
            });

            // Build the prompt — dream agent's startup_instructions + the filtered log
            const systemPrompt = dreamAgent.startup_instructions || '';
            const userMessage = 'Conversation logs for agent "' + agent.name + '":\n\n'
                + filtered
                + '\n\nAlso provide a brief title summarizing the overarching subject of the day.';

            // Call the dream agent's LLM
            const apiKey = decryptApiKey(dreamAgent.api_key);
            const conf = buildConf(dreamAgent);
            const providerFn = createProvider(dreamAgent.provider, dreamAgent.model, apiKey, conf);
            const { text: response, usage } = await providerFn(systemPrompt, userMessage);

            // Record cost against the dream agent
            await recordDreamUsage(dreamAgent.agent, dreamAgent.provider, dreamAgent.model, usage);

            // Parse the response — extract title and content
            // Expected: the LLM provides a title line and then the consolidated content
            const titleMatch = response.match(/^#\s+(.+)$/m) || response.match(/^title:\s*(.+)$/im);
            let title = titleMatch ? titleMatch[1].trim() : 'Dream consolidation';
            let content = response;

            // Save as a note in the agent's namespace
            const dateStr = new Date().toISOString().slice(0, 10);
            const slug = 'dreams/' + dateStr + '-' + slugify(title);

            await saveNote(agent.name, title + ' (' + dateStr + ')', content, slug, dreamAgent.agent);

            logDream('saved', { agent: agent.name, slug, titleLength: title.length, contentLength: content.length });

            // Update last_dream_at
            await pool.query(
                'UPDATE agent_configuration SET last_dream_at = NOW() WHERE actor_id = $1',
                [agent.actor_id]
            );

            results.push({
                agent: agent.name,
                mode: agent.dream_mode,
                slug,
                title,
                logCount: logs.rows.length,
                filteredSize: filtered.length,
                responseSize: response.length,
                tokens: (usage.input_tokens || 0) + (usage.output_tokens || 0)
            });
        } catch (err) {
            logDream('error', { agent: agent.name, error: err.message });
            results.push({ agent: agent.name, error: err.message });
        }
    }

    logDream('complete', { processed: results.length });
    return { processed: results.length, results };
}

// Start the dream scheduler. Reads dream_cron_schedule from config
// and schedules runDream() accordingly. Called once at server startup.
let scheduledTask = null;

function startDreamScheduler() {
    const cron = require('node-cron');
    const schedule = config.get('dream_cron_schedule') || '';

    if (!schedule) {
        logDream('scheduler', { message: 'No dream_cron_schedule configured, scheduler disabled' });
        return;
    }

    if (!cron.validate(schedule)) {
        logDream('scheduler-error', { message: 'Invalid cron expression: ' + schedule });
        return;
    }

    // Stop any existing scheduled task (in case of hot reload)
    if (scheduledTask) {
        scheduledTask.stop();
    }

    scheduledTask = cron.schedule(schedule, async () => {
        logDream('cron-trigger', { schedule });
        try {
            const result = await runDream();
            logDream('cron-complete', { result });
        } catch (err) {
            logDream('cron-error', { error: err.message });
        }
    });

    logDream('scheduler', { message: 'Dream scheduler started', schedule });
}

module.exports = { runDream, prefilterLog, startDreamScheduler };
