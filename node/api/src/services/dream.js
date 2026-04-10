// Dream processing — nightly conversation log analysis.
// Reads conversation logs uploaded by agents, sends them through a dream
// virtual agent (companion or technical), and saves consolidated insights
// as notes in the agent's namespace.

const pool = require('../db');
const config = require('./config');
const { log, logError } = require('./logger');
const { saveNote, readNote } = require('./documents');
const { invokeAgent } = require('./virtual-agent');

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

// Find a dream agent by expertise tag. Verifies it exists, is owned by system
// or by a user with 'agents/create_system_equivalent' permission, and has
// provider/model/api_key configured. Returns the agent name or null.
async function findDreamAgent(expertiseTag) {
    const { isTrustedCreator } = require('./admin-permissions');

    const result = await pool.query(
        `SELECT ac.id, ac.name, ac.created_by, agc.provider, agc.model, agc.api_key
         FROM actors ac
         JOIN agent_configuration agc ON agc.actor_id = ac.id
         WHERE ac.expertise @> jsonb_build_array($1::text)`,
        [expertiseTag]
    );

    if (result.rows.length === 0) {
        logDream('error', { message: 'No agent found with expertise: ' + expertiseTag });
        return null;
    }

    // Verify ownership — must be created by system or a trusted creator
    let agent = null;
    for (const row of result.rows) {
        if (await isTrustedCreator(row.created_by)) {
            agent = row;
            break;
        }
    }

    if (!agent) {
        logDream('error', { message: 'Agent with expertise "' + expertiseTag + '" not owned by system or trusted creator' });
        return null;
    }

    if (!agent.api_key || !agent.provider || !agent.model) {
        logDream('error', { message: agent.name + ' (expertise: ' + expertiseTag + ') missing provider/model/api_key' });
        return null;
    }

    return agent.name;
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

    // Find dream agents by expertise tag
    const companionAgentName = await findDreamAgent('dream-companion');
    const technicalAgentName = await findDreamAgent('dream-technical');
    const companionSoulAgentName = await findDreamAgent('dream-companion-soul');
    const technicalSoulAgentName = await findDreamAgent('dream-technical-soul');

    if (!companionAgentName && !technicalAgentName) {
        logDream('abort', { reason: 'Neither dream agent found or valid' });
        return { error: 'No valid dream agents found. Both dream-companion and dream-technical must exist and be created by a trusted creator.' };
    }

    // Find agents with dream mode enabled
    const agents = await pool.query(
        `SELECT ac.name, ac.id AS actor_id, agc.dream_mode, agc.last_dream_at
         FROM agent_configuration agc
         JOIN actors ac ON ac.id = agc.actor_id
         WHERE agc.dream_mode IN ('companion', 'technical')`
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
            const dreamAgentName = agent.dream_mode === 'companion' ? companionAgentName : technicalAgentName;
            if (!dreamAgentName) {
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

            // Call the dream agent's LLM via invokeAgent
            // Uses the dream VA's startup_instructions as system prompt (default behavior)
            const userMessage = 'Conversation logs for agent "' + agent.name + '":\n\n'
                + filtered
                + '\n\nAlso provide a brief title summarizing the overarching subject of the day.';

            const { text: response } = await invokeAgent(dreamAgentName, {
                userMessage,
                context: 'dream',
                skipRateLimit: true,
                skipCostLimit: true,
                skipRetry: false,
            });

            // Parse the response — extract title and content
            // Expected: the LLM provides a title line and then the consolidated content
            const titleMatch = response.match(/^#\s+(.+)$/m) || response.match(/^title:\s*(.+)$/im);
            let title = titleMatch ? titleMatch[1].trim() : 'Dream consolidation';
            let content = response;

            // Save as a note in the agent's namespace
            const dateStr = new Date().toISOString().slice(0, 10);
            const slug = 'dreams/' + dateStr + '-' + slugify(title);

            await saveNote(agent.name, title + ' (' + dateStr + ')', content, slug, dreamAgentName);

            logDream('saved', { agent: agent.name, slug, titleLength: title.length, contentLength: content.length });

            // Create graph relations from source conversations to the dream note.
            // Each conversation that fed this dream gets a "led-to" edge.
            // Per-item try/catch so one bad row doesn't abort the rest.
            {
                const { createRelation } = require('./relations');
                const sourceSlugs = [...new Set(logs.rows.map(r => r.slug).filter(Boolean))];
                let relCreated = 0;

                for (const sourceSlug of sourceSlugs) {
                    try {
                        await createRelation(
                            agent.name, sourceSlug,
                            agent.name, slug,
                            'led-to',
                            dreamAgentName,
                            { source: 'dream-processing' },
                            true // auto_extracted
                        );
                        relCreated++;
                    } catch (relErr) {
                        logDream('relation-error', {
                            agent: agent.name,
                            dreamSlug: slug,
                            sourceSlug: sourceSlug,
                            error: relErr.message
                        });
                    }
                }

                logDream('relations-created', { agent: agent.name, dreamSlug: slug, sourceCount: sourceSlugs.length, createdCount: relCreated });
            }

            // Soul synthesis: update context/soul with tonight's snapshot
            const soulAgentName = agent.dream_mode === 'companion' ? companionSoulAgentName : technicalSoulAgentName;
            if (soulAgentName) {
                try {
                    let existingSoul = '';
                    try {
                        const soulNote = await readNote(agent.name, 'context/soul');
                        existingSoul = soulNote.content || '';
                    } catch (e) {
                        // No soul yet — first run, start fresh
                    }

                    const soulUserMessage = '## Current soul document\n\n'
                        + (existingSoul || '(empty — first run)')
                        + '\n\n## Tonight\'s dream snapshot\n\n'
                        + content;

                    const { text: updatedSoul } = await invokeAgent(soulAgentName, {
                        userMessage: soulUserMessage,
                        context: 'soul',
                        skipRateLimit: true,
                        skipCostLimit: true,
                        skipRetry: false,
                    });

                    if (updatedSoul && updatedSoul.trim()) {
                        await saveNote(agent.name, 'Soul', updatedSoul.trim(), 'context/soul', soulAgentName);
                        logDream('soul-updated', { agent: agent.name, size: updatedSoul.length });
                    }
                } catch (soulErr) {
                    // Soul update failure shouldn't block the rest of the dream process
                    logDream('soul-error', { agent: agent.name, error: soulErr.message });
                }
            }

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
            });
        } catch (err) {
            logDream('error', { agent: agent.name, error: err.message });
            results.push({ agent: agent.name, error: err.message });
        }

        // Delay between agents to avoid hammering the provider
        const interDelay = parseInt(config.get('dream_interagent_delay')) || 2000;
        if (interDelay > 0) {
            await new Promise(resolve => setTimeout(resolve, interDelay));
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
            logError('dream', 'cron-error', { message: err.message, detail: err.stack });
        }
    });

    logDream('scheduler', { message: 'Dream scheduler started', schedule });
}

module.exports = { runDream, prefilterLog, startDreamScheduler };
