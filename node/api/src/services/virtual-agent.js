// Virtual agent service — handles discussion participation for API-backed agents.
// Triggered by the system message handler when a message of type 'virtual-agent' arrives.
// Looks up discussion context, identifies virtual participants, builds prompts,
// calls provider APIs, and posts responses as chat messages.

const pool = require('../db');
const { log } = require('./logger');
const { searchMemory } = require('./memory');
const { createProvider, decryptApiKey } = require('./provider');
const { broadcast } = require('./events');

function logVA(action, details) {
    log('virtual-agent', action, details);
}

// Load an agent row with virtual-agent fields.
async function loadAgent(agentName) {
    const result = await pool.query(
        'SELECT agent, virtual, provider, model, api_key, configuration, startup_instructions, personality, expertise FROM agents WHERE agent = $1',
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
        'SELECT agent, status, role FROM discussion_participants WHERE discussion_id = $1',
        [discussionId]
    );

    return { discussion: disc.rows[0], participants: parts.rows };
}

// Get recent chat history for a discussion channel, excluding system-to-system triggers.
async function loadChatHistory(channel, limit) {
    const result = await pool.query(
        `SELECT from_agent, to_agent, message, sent_at FROM chat_messages
         WHERE channel = $1 AND NOT (from_agent = 'system' AND to_agent = 'system')
         ORDER BY id DESC LIMIT $2`,
        [channel, limit || 50]
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
function buildSystemPrompt(agent, discussion, ragContext) {
    let prompt = '';

    // Agent's own instructions (set via save_instructions)
    if (agent.startup_instructions) {
        prompt += agent.startup_instructions + '\n\n';
    }

    // Discussion context
    prompt += `You are "${agent.agent}", a participant in discussion #${discussion.id}.\n`;
    prompt += `Topic: ${discussion.topic}\n`;
    if (discussion.context) {
        prompt += `Context: ${discussion.context}\n`;
    }
    prompt += `Mode: ${discussion.mode}\n\n`;

    // RAG context
    if (ragContext) {
        prompt += 'Relevant knowledge from your notes:\n' + ragContext + '\n\n';
    }

    prompt += 'Respond concisely and stay on topic. You are participating in a multi-agent discussion.';

    return prompt;
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

// Post an error message to the discussion channel from the virtual agent.
async function postError(agentName, channel, error) {
    try {
        await pool.query(
            'INSERT INTO chat_messages (from_agent, to_agent, message, channel) VALUES ($1, $2, $3, $4)',
            [agentName, '*', `[Error: ${error}]`, channel]
        );
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
    // Check if already voted
    const existing = await pool.query(
        'SELECT 1 FROM discussion_ballots WHERE vote_id = $1 AND agent = $2',
        [voteId, agentName]
    );
    if (existing.rows.length > 0) {
        logVA('already-voted', { voteId, agent: agentName });
        return;
    }

    await pool.query(
        'INSERT INTO discussion_ballots (vote_id, agent, choice, reason) VALUES ($1, $2, $3, $4)',
        [voteId, agentName, choice, reason]
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

    const channel = discussion.channel || `discuss-${discussionId}`;

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
                await postError(agent.agent, channel, 'No API key configured');
                continue;
            }
            if (!agent.provider || !agent.model) {
                await postError(agent.agent, channel, 'No provider/model configured');
                continue;
            }

            // Decrypt API key
            const apiKey = decryptApiKey(agent.api_key);

            // Parse configuration
            let conf = {};
            if (agent.configuration) {
                try { conf = JSON.parse(agent.configuration); } catch (e) { /* use defaults */ }
            }

            // RAG context
            const ragContext = await loadRAGContext(agent.agent, discussion.topic);

            // Build prompts
            const systemPrompt = buildSystemPrompt(agent, discussion, ragContext);
            const userMessage = buildUserMessage(chatHistory, triggerType, voteQuestion);

            // Call provider — show activity spinner while the AI call is in flight
            await pool.query('UPDATE agents SET active_since = NOW() WHERE agent = $1', [agent.agent]);
            broadcast('agent_activity', { agent: agent.agent, active: true });
            const provider = createProvider(agent.provider, agent.model, apiKey, conf);
            let response;
            try {
                response = await provider(systemPrompt, userMessage);
            } finally {
                await pool.query('UPDATE agents SET active_since = NULL WHERE agent = $1', [agent.agent]);
                broadcast('agent_activity', { agent: agent.agent, active: false });
            }

            // Handle vote-proposed: parse response and cast ballot
            if (triggerType === 'vote-proposed' && voteId) {
                const voteResponse = parseVoteResponse(response);
                if (voteResponse) {
                    await castVote(voteId, agent.agent, voteResponse.choice, voteResponse.reason);
                    // Also post the reasoning as a chat message
                    if (voteResponse.reason) {
                        await pool.query(
                            'INSERT INTO chat_messages (from_agent, to_agent, message, channel) VALUES ($1, $2, $3, $4)',
                            [agent.agent, '*', voteResponse.reason, channel]
                        );
                    }
                } else {
                    // Couldn't parse vote, post response as chat and log
                    logVA('vote-parse-failed', { discussionId, agent: agent.agent });
                    await pool.query(
                        'INSERT INTO chat_messages (from_agent, to_agent, message, channel) VALUES ($1, $2, $3, $4)',
                        [agent.agent, '*', response, channel]
                    );
                }
            } else {
                // Regular message response
                await pool.query(
                    'INSERT INTO chat_messages (from_agent, to_agent, message, channel) VALUES ($1, $2, $3, $4)',
                    [agent.agent, '*', response, channel]
                );
            }

            logVA('responded', { discussionId, agent: agent.agent, triggerType, responseLength: response.length });

        } catch (err) {
            logVA('agent-error', { discussionId, agent: agent.agent, error: err.message });
            await postError(agent.agent, channel, err.message);
        }
    }
}

// Register with system handler on load.
const systemHandler = require('./system-handler');
systemHandler.register('virtual-agent', handleVirtualAgent);

module.exports = { handleVirtualAgent };
