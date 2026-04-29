// Dream processing — nightly conversation log analysis.
// Reads conversation logs uploaded by agents, sends them through a dream
// virtual agent (companion or technical), and saves consolidated insights
// as notes in the agent's namespace.

const pool = require('../db');
const config = require('./config');
const { log, logError } = require('./logger');
const { saveNote, readNote, listNotes } = require('./documents');
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

// Cheap detection for the typed-context JSON array format. The whole
// multi-turn discussion history sits on a single line as a JSON array
// of {sender, content} objects (see virtual-agent.js's <Conversation>
// block). prefilterLog and extractSpeakers both need this pattern —
// prefilterLog so the speaker record survives signal-based filtering,
// extractSpeakers so it can parse the array into per-speaker lines.
const JSON_ARRAY_HEAD = /^\s*\[\s*\{\s*"sender"\s*:/;


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

    // Always include the typed-context JSON-array conversation record. The
    // array is one long line carrying every speaker's turn, but its
    // utterances often don't contain signal-pattern words, so signal-only
    // filtering can drop it entirely. When that happens extractSpeakers
    // sees no parseable speaker data and the people-update loop runs zero
    // iterations — the visible symptom is per-NPC people-files freezing in
    // place after a multi-agent discussion. Pinning these lines into the
    // filtered output guarantees extractSpeakers always sees the speaker
    // enumeration, while the dream LLM still gets the signal-filtered
    // version of the surrounding narrative.
    for (let i = 0; i < lines.length; i++) {
        if (JSON_ARRAY_HEAD.test(lines[i])) {
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

// Extract unique speakers from conversation logs and group lines by speaker.
// Handles four formats:
//   memory-sync uploads:    "[HH:MM speaker] message text"
//   VA transcript metadata: "- **From:** speaker"
//   discussion history:     "speaker: message text" or "[timestamp] speaker: message text"
//   typed-context-injection JSON array: '[{"sender":"name","content":"..."}, ...]'
//     — produced by the typed-context VA prompt (the <Discussion> block in
//     virtual-agent.js). The whole message history sits on one line as a
//     JSON array; we parse it and treat each entry as a discrete speaker
//     line. Without this branch the salem NPCs' people-files freeze the
//     moment their conversation traffic shifts to multi-agent discussions.
// Returns a Map of speaker name → array of relevant lines.
function extractSpeakers(content, agentName) {
    const lines = content.split('\n');
    const speakerLines = new Map();
    const agentLower = agentName.toLowerCase();

    // Pattern for memory-sync format: [HH:MM speaker]
    const chatPattern = /^\[(\d{2}:\d{2})\s+(\S+)\]/;
    // Pattern for VA transcript metadata: - **From:** speaker
    const fromPattern = /^-\s+\*\*From:\*\*\s+(\S+)/;
    // Pattern for discussion history: "speaker: message" or "[timestamp] speaker: message"
    // Speaker names are agent identifiers (lowercase, may contain hyphens)
    const discussionPattern = /^(?:\[.*?\]\s+)?([a-z][a-z0-9-]*):(?:\s|$)/;

    let currentSpeaker = null;

    function addSpeaker(name, line) {
        const lower = name.toLowerCase();
        if (lower === agentLower) {
            currentSpeaker = null;
            return;
        }
        currentSpeaker = lower;
        if (!speakerLines.has(lower)) {
            speakerLines.set(lower, []);
        }
        if (line) {
            speakerLines.get(lower).push(line);
        }
    }

    for (const line of lines) {
        // Skip section headers and metadata
        if (line.startsWith('##') || line.startsWith('---')) {
            continue;
        }

        // Typed-context JSON array — handle before the chat/discussion
        // patterns because the line opens with '[' and could otherwise
        // confuse the timestamp-prefixed discussionPattern. Failure to
        // parse falls through to the line-based patterns below.
        if (JSON_ARRAY_HEAD.test(line)) {
            try {
                const messages = JSON.parse(line.trim());
                if (Array.isArray(messages)) {
                    for (const msg of messages) {
                        if (!msg || typeof msg.sender !== 'string') {
                            continue;
                        }
                        const sender = msg.sender;
                        const lower = sender.toLowerCase();
                        if (lower === agentLower) {
                            continue;
                        }
                        if (!speakerLines.has(lower)) {
                            speakerLines.set(lower, []);
                        }
                        const text = typeof msg.content === 'string' ? msg.content : '';
                        // Label each message with the speaker so the
                        // dream-companion-people LLM can tell turns apart
                        // when several get joined into one prompt.
                        speakerLines.get(lower).push('[' + sender + '] ' + text);
                    }
                    // Reset currentSpeaker — the JSON array is its own
                    // self-contained block; don't let stray lines after
                    // it attach to the last sender from the array.
                    currentSpeaker = null;
                    continue;
                }
            } catch (err) {
                // Not a parseable array — fall through to other patterns.
            }
        }

        const chatMatch = line.match(chatPattern);
        if (chatMatch) {
            addSpeaker(chatMatch[2], line);
            continue;
        }

        const fromMatch = line.match(fromPattern);
        if (fromMatch) {
            addSpeaker(fromMatch[1], null);
            continue;
        }

        const discussionMatch = line.match(discussionPattern);
        if (discussionMatch) {
            addSpeaker(discussionMatch[1], line);
            continue;
        }

        // Continuation lines belong to the current speaker
        if (currentSpeaker && line.trim()) {
            if (speakerLines.has(currentSpeaker)) {
                speakerLines.get(currentSpeaker).push(line);
            }
        }
    }

    return speakerLines;
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

// Split a [since, now] window into per-UTC-day chunks. The first chunk
// starts at `since` (not at the start of that UTC day) so we don't
// re-scan logs already consumed by a prior cron run. Subsequent chunks
// are full UTC days. The final chunk ends at `now`.
//
// Returns [{from: Date, to: Date}, ...]. Empty if since >= now.
//
// Bounds are inclusive on `to`, exclusive on `from`, matching the
// per-chunk SQL query (created_at > from AND created_at <= to). This
// makes setting last_dream_at = chunk.to safely exclude already-
// processed boundary-time logs from the next chunk's window.
function computeDailyChunks(since, now) {
    const sinceMs = (since instanceof Date ? since : new Date(since)).getTime();
    const nowMs = (now instanceof Date ? now : new Date(now)).getTime();
    if (sinceMs >= nowMs) {
        return [];
    }
    const chunks = [];
    const sinceDate = new Date(sinceMs);
    let dayEndMs = Date.UTC(
        sinceDate.getUTCFullYear(),
        sinceDate.getUTCMonth(),
        sinceDate.getUTCDate()
    ) + 24 * 60 * 60 * 1000;
    let cursorMs = sinceMs;
    while (cursorMs < nowMs) {
        const chunkEndMs = Math.min(dayEndMs, nowMs);
        chunks.push({ from: new Date(cursorMs), to: new Date(chunkEndMs) });
        cursorMs = chunkEndMs;
        dayEndMs += 24 * 60 * 60 * 1000;
    }
    return chunks;
}

// Process one (from, to] chunk for one agent: dream → save note → soul →
// people. Returns a summary object. Throws on dream-call failure (the
// caller catches and decides whether to retry on the next cron). Soul
// and people failures are caught here and logged but don't fail the
// chunk — they're auxiliary to the dream note itself.
//
// agentNames: { dreamAgentName, soulAgentName, peopleAgentName }
// chunk: { from: Date, to: Date }
async function processDreamChunk(agent, agentNames, chunk) {
    const { dreamAgentName, soulAgentName, peopleAgentName } = agentNames;
    const { from, to } = chunk;
    const chunkDateStr = from.toISOString().slice(0, 10);

    const logs = await pool.query(
        `SELECT slug, content, created_at FROM documents
         WHERE namespace = $1 AND slug LIKE 'conversations/%' AND deleted_at IS NULL
         AND created_at > $2 AND created_at <= $3
         ORDER BY created_at ASC`,
        [agent.name, from, to]
    );
    if (logs.rows.length === 0) {
        logDream('chunk-no-logs', { agent: agent.name, chunkDate: chunkDateStr });
        return { skipped: true, reason: 'no logs', chunkDate: chunkDateStr };
    }

    const fullLog = logs.rows.map(r => r.content).join('\n\n---\n\n');
    const filtered = prefilterLog(fullLog);
    if (!filtered) {
        logDream('chunk-no-signals', { agent: agent.name, chunkDate: chunkDateStr, logCount: logs.rows.length });
        return { skipped: true, reason: 'no signals', chunkDate: chunkDateStr, logCount: logs.rows.length };
    }

    logDream('chunk-processing', {
        agent: agent.name,
        mode: agent.dream_mode,
        chunkDate: chunkDateStr,
        logCount: logs.rows.length,
        originalSize: fullLog.length,
        filteredSize: filtered.length,
    });

    const userMessage = 'Conversation logs for agent "' + agent.name + '" on ' + chunkDateStr + ':\n\n'
        + filtered
        + '\n\nAlso provide a brief title summarizing the overarching subject of the day.';

    const { text: response } = await invokeAgent(dreamAgentName, {
        userMessage,
        context: 'dream',
        skipRateLimit: true,
        skipCostLimit: true,
        skipRetry: false,
    });

    const titleMatch = response.match(/^#\s+(.+)$/m) || response.match(/^title:\s*(.+)$/im);
    const title = titleMatch ? titleMatch[1].trim() : 'Dream consolidation';
    const content = response;

    // Slug uses the chunk's date so catching up multiple days produces
    // distinct dated notes (rather than overwriting the same NOW-dated slug).
    const slug = 'dreams/' + chunkDateStr + '-' + slugify(title);
    await saveNote(agent.name, title + ' (' + chunkDateStr + ')', content, slug, dreamAgentName);
    logDream('chunk-saved', { agent: agent.name, slug, contentLength: content.length });

    // Soul synthesis — runs after each chunk per Jeff's call. The current
    // soul note is the prior chunk's output, so consecutive chunks build
    // on each other naturally rather than needing a single end-of-run pass.
    if (soulAgentName) {
        try {
            let existingSoul = '';
            try {
                const soulNote = await readNote(agent.name, 'context/soul');
                existingSoul = soulNote.content || '';
            } catch (e) {
                // No soul yet — first run for this agent.
            }

            // When the soul is empty (deleted or first run), backload the N
            // most recent dreams instead of just feeding the chunk we just
            // saved. Lets a deleted soul rebuild from accumulated personality
            // rather than starting flat and slowly filling in over many
            // cycles. The just-saved chunk is included as the first entry
            // since listNotes orders by updated_at DESC. After this call
            // existingSoul will be non-empty so subsequent cycles resume the
            // normal per-chunk update path. Cost guards on the soul agent
            // call protect against runaway prompt sizes.
            let backloadDreams = null;
            const soulIsEmpty = existingSoul.trim() === '';
            if (soulIsEmpty) {
                let backloadCount = 0;
                try {
                    backloadCount = parseInt(config.get('dream_backload_count'), 10) || 0;
                } catch (e) {
                    // Config key missing (deploy ordering: service started before
                    // migration ran). Treat as disabled rather than crash.
                    backloadCount = 0;
                }
                // Hard cap at 20 regardless of config — sanity bound on the
                // sequential read burst.
                backloadCount = Math.max(0, Math.min(backloadCount, 20));
                if (backloadCount > 0) {
                    const list = await listNotes(agent.name, backloadCount, 0, 'dreams/');
                    if (list.notes && list.notes.length > 0) {
                        const dreamReads = await Promise.all(
                            list.notes.map(n => readNote(agent.name, n.slug).catch(() => null))
                        );
                        backloadDreams = dreamReads
                            .filter(d => d && d.content)
                            .map(d => `### ${d.slug}\n\n${d.content}`)
                            .join('\n\n---\n\n');
                    }
                }
            }

            const soulUserMessage = '## Agent: ' + agent.name + '\n\n'
                + (agent.startup_instructions
                    ? '## Character description\n\n' + agent.startup_instructions + '\n\n'
                    : '')
                + '## Current soul document\n\n'
                + (soulIsEmpty ? '(empty — first run)' : existingSoul)
                + (backloadDreams
                    ? '\n\n## Dream snapshot for initial soul rebuild\n\n'
                        + 'The current soul document is empty. Synthesize an initial soul from the recent dream history below; do not treat this as a single-day incremental update.\n\n'
                        + backloadDreams
                    : '\n\n## Dream snapshot for ' + chunkDateStr + '\n\n' + content);

            const { text: updatedSoul } = await invokeAgent(soulAgentName, {
                userMessage: soulUserMessage,
                context: 'soul',
                skipRateLimit: true,
                skipCostLimit: true,
                skipRetry: false,
            });

            if (updatedSoul && updatedSoul.trim()) {
                await saveNote(agent.name, 'Soul', updatedSoul.trim(), 'context/soul', soulAgentName, null, null, { upsert: true });
                logDream('chunk-soul-updated', { agent: agent.name, chunkDate: chunkDateStr, size: updatedSoul.length });
            }
        } catch (soulErr) {
            // Soul failure doesn't block the chunk's dream/people output.
            logDream('chunk-soul-error', { agent: agent.name, chunkDate: chunkDateStr, error: soulErr.message });
        }
    }

    // People synthesis — companion mode only. Runs per-chunk for the same
    // reason soul does: per-day relationship updates compose better than
    // one massive end-of-run pass over weeks of conversation.
    if (agent.dream_mode === 'companion' && peopleAgentName) {
        try {
            const speakers = extractSpeakers(filtered, agent.name);
            for (const [personName, personLines] of speakers) {
                if (personLines.length === 0) {
                    continue;
                }
                try {
                    let existingFile = '';
                    try {
                        const note = await readNote(agent.name, 'context/people/' + personName);
                        existingFile = note.content || '';
                    } catch (e) {
                        // No existing file — first encounter.
                    }

                    const peopleUserMessage = '## Agent: ' + agent.name + '\n'
                        + '## Person: ' + personName + '\n'
                        + '## Today\'s date: ' + chunkDateStr + '\n\n'
                        + '## Current relationship file\n\n'
                        + (existingFile || '(empty — first encounter)')
                        + '\n\n## Recent conversation excerpts involving ' + personName + '\n\n'
                        + personLines.join('\n');

                    const { text: updatedFile } = await invokeAgent(peopleAgentName, {
                        userMessage: peopleUserMessage,
                        context: 'people',
                        skipRateLimit: true,
                        skipCostLimit: true,
                        skipRetry: false,
                    });

                    if (updatedFile && updatedFile.trim()) {
                        await saveNote(
                            agent.name,
                            'People — ' + personName,
                            updatedFile.trim(),
                            'context/people/' + personName,
                            peopleAgentName,
                            null, null, { upsert: true }
                        );
                        logDream('chunk-people-updated', {
                            agent: agent.name,
                            chunkDate: chunkDateStr,
                            person: personName,
                            size: updatedFile.length,
                        });
                    }
                } catch (personErr) {
                    logDream('chunk-people-error', {
                        agent: agent.name,
                        chunkDate: chunkDateStr,
                        person: personName,
                        error: personErr.message,
                    });
                }
            }
        } catch (peopleErr) {
            logDream('chunk-people-error', { agent: agent.name, chunkDate: chunkDateStr, error: peopleErr.message });
        }
    }

    return {
        processed: true,
        chunkDate: chunkDateStr,
        slug,
        title,
        logCount: logs.rows.length,
        filteredSize: filtered.length,
        responseSize: response.length,
    };
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
    const simAgentName = await findDreamAgent('dream-sim');
    const companionSoulAgentName = await findDreamAgent('dream-companion-soul');
    const technicalSoulAgentName = await findDreamAgent('dream-technical-soul');
    const simSoulAgentName = await findDreamAgent('dream-sim-soul');
    const companionPeopleAgentName = await findDreamAgent('dream-companion-people');
    const simPeopleAgentName = await findDreamAgent('dream-sim-people');

    if (!companionAgentName && !technicalAgentName && !simAgentName) {
        logDream('abort', { reason: 'No dream agent found or valid' });
        return { error: 'No valid dream agents found. At least one of dream-companion, dream-technical, or dream-sim must exist and be created by a trusted creator.' };
    }

    // Find agents with dream mode enabled
    const agents = await pool.query(
        `SELECT ac.name, ac.id AS actor_id, agc.dream_mode, agc.last_dream_at,
                agc.startup_instructions
         FROM agent_configuration agc
         JOIN actors ac ON ac.id = agc.actor_id
         WHERE agc.dream_mode IN ('companion', 'technical', 'sim')`
    );

    if (agents.rows.length === 0) {
        logDream('skip', { reason: 'No agents with dream mode enabled' });
        return { processed: 0, reason: 'No agents with dream mode enabled' };
    }

    logDream('start', { agents: agents.rows.map(a => a.name + ':' + a.dream_mode) });

    const results = [];

    for (const agent of agents.rows) {
        try {
            // Pick the right dream/soul/people agents for this dream_mode.
            let dreamAgentName = null;
            let soulAgentName = null;
            let peopleAgentName = null;
            if (agent.dream_mode === 'companion') {
                dreamAgentName = companionAgentName;
                soulAgentName = companionSoulAgentName;
                peopleAgentName = companionPeopleAgentName;
            } else if (agent.dream_mode === 'technical') {
                dreamAgentName = technicalAgentName;
                soulAgentName = technicalSoulAgentName;
            } else if (agent.dream_mode === 'sim') {
                dreamAgentName = simAgentName;
                soulAgentName = simSoulAgentName;
                peopleAgentName = simPeopleAgentName;
            }
            if (!dreamAgentName) {
                results.push({ agent: agent.name, error: 'dream-' + agent.dream_mode + ' agent not available' });
                continue;
            }
            const agentNames = { dreamAgentName, soulAgentName, peopleAgentName };

            // Split the work since last_dream_at into per-UTC-day chunks so an
            // agent that's fallen behind doesn't try to fit weeks of logs into
            // one model call (which is what tripped home with deepseek's 163K
            // window). First-run agents process the previous 24h.
            const since = agent.last_dream_at || new Date(Date.now() - 24 * 60 * 60 * 1000);
            const chunks = computeDailyChunks(since, new Date());

            if (chunks.length === 0) {
                logDream('no-window', { agent: agent.name, since });
                results.push({ agent: agent.name, skipped: true, reason: 'last_dream_at is in the future' });
                continue;
            }

            logDream('chunks-planned', {
                agent: agent.name,
                count: chunks.length,
                from: chunks[0].from.toISOString(),
                to: chunks[chunks.length - 1].to.toISOString(),
            });

            const interChunkDelay = parseInt(config.get('dream_interchunk_delay')) || 1000;
            const chunkResults = [];

            for (let i = 0; i < chunks.length; i++) {
                const chunk = chunks[i];
                try {
                    const r = await processDreamChunk(agent, agentNames, chunk);
                    chunkResults.push(r);
                    // Advance last_dream_at after each successful chunk so a
                    // failure on a later chunk doesn't lose the work done on
                    // earlier ones — the next cron resumes from where we
                    // stopped, not from the start of the agent's backlog.
                    await pool.query(
                        'UPDATE agent_configuration SET last_dream_at = $1 WHERE actor_id = $2',
                        [chunk.to, agent.actor_id]
                    );
                } catch (chunkErr) {
                    // Don't advance last_dream_at — next cron retries this
                    // chunk. Stop processing this agent's later chunks so we
                    // don't skip past a failed one (would lose its logs).
                    logDream('chunk-error', {
                        agent: agent.name,
                        chunkDate: chunk.from.toISOString().slice(0, 10),
                        error: chunkErr.message,
                    });
                    logError('dream', 'chunk-error', {
                        agent: agent.name,
                        message: chunkErr.message,
                        detail: chunkErr.stack,
                    });
                    chunkResults.push({
                        chunkDate: chunk.from.toISOString().slice(0, 10),
                        error: chunkErr.message,
                    });
                    break;
                }
                // Inter-chunk pause for the same agent — politeness to the
                // provider when catching up multiple days back-to-back.
                if (i + 1 < chunks.length && interChunkDelay > 0) {
                    await new Promise(resolve => setTimeout(resolve, interChunkDelay));
                }
            }

            results.push({
                agent: agent.name,
                mode: agent.dream_mode,
                chunkCount: chunks.length,
                chunks: chunkResults,
            });
        } catch (err) {
            logDream('error', { agent: agent.name, error: err.message });
            // Also surface in the admin error_log so per-agent failures aren't
            // silently swallowed (the outer cron-level catch only fires if
            // runDream itself throws, not for individual agents).
            logError('dream', 'agent-error', {
                agent: agent.name,
                message: err.message,
                detail: err.stack,
            });
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

module.exports = { runDream, prefilterLog, extractSpeakers, startDreamScheduler };
