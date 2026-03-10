// Service layer for chat operations (send, receive, ack, status).
// Extracted from routes/chat.js so both REST routes and MCP handler can share the same logic.

const pool = require('../db');
const { log } = require('./logger');
const systemHandler = require('./system-handler');

const CHANNEL_PATTERN = /^[a-zA-Z0-9_-]{1,50}$/;

function validateChannel(channel) {
    if (channel === undefined || channel === null) {
        return null;
    }
    if (typeof channel !== 'string' || !CHANNEL_PATTERN.test(channel)) {
        throw Object.assign(new Error('Invalid channel: must match /^[a-zA-Z0-9_-]{1,50}$/'), { statusCode: 400 });
    }
    return channel;
}

function logChat(action, details) {
    log('chat', action, details);
}

async function chatSend(fromAgent, toAgents, discussionId, message, channel) {
    if (!fromAgent || !message) {
        throw Object.assign(new Error('Required fields: from_agent, message'), { statusCode: 400 });
    }
    if (!toAgents && !discussionId) {
        throw Object.assign(new Error('Required: to_agents (array) or discussion_id'), { statusCode: 400 });
    }

    // Auto-derive channel from discussion_id when not explicitly provided
    const ch = validateChannel(channel || (discussionId ? `discussion-${discussionId}` : undefined));

    const discussionParticipants = new Set();
    const recipientSet = new Set();

    if (discussionId) {
        const participants = await pool.query(
            'SELECT agent FROM discussion_participants WHERE discussion_id = $1 AND status = $2 AND agent != $3',
            [discussionId, 'joined', fromAgent]
        );
        for (const row of participants.rows) {
            discussionParticipants.add(row.agent);
            recipientSet.add(row.agent);
        }
    }

    if (toAgents && toAgents.length === 1 && toAgents[0] === '*') {
        const known = await pool.query('SELECT agent FROM agents WHERE agent != $1', [fromAgent]);
        for (const row of known.rows) {
            recipientSet.add(row.agent);
        }
    } else if (toAgents) {
        if (!Array.isArray(toAgents) || toAgents.length === 0) {
            throw Object.assign(new Error('to_agents must be a non-empty array'), { statusCode: 400 });
        }
        for (const agent of toAgents) {
            recipientSet.add(agent);
        }
    }

    recipientSet.delete(fromAgent);
    const recipients = Array.from(recipientSet);
    if (recipients.length === 0) {
        throw Object.assign(new Error('No recipients resolved'), { statusCode: 400 });
    }

    for (const agent of recipients) {
        const exists = await pool.query('SELECT 1 FROM agents WHERE agent = $1', [agent]);
        if (exists.rows.length === 0) {
            throw Object.assign(new Error(`Agent "${agent}" is not registered`), { statusCode: 404 });
        }
    }

    const results = [];
    for (const recipient of recipients) {
        let msgText = message;
        if (discussionId && !discussionParticipants.has(recipient)) {
            msgText = `[Forwarded from discussion #${discussionId}] ${message}`;
        }
        const result = await pool.query(
            'INSERT INTO chat_messages (from_agent, to_agent, message, channel) VALUES ($1, $2, $3, $4) RETURNING id, sent_at',
            [fromAgent, recipient, msgText, ch]
        );
        results.push({ id: result.rows[0].id, agent: recipient, sent_at: result.rows[0].sent_at });
    }

    logChat('send', { from_agent: fromAgent, to_agents: recipients, message_ids: results.map(r => r.id), channel: ch, discussion_id: discussionId || null });

    // Fire-and-forget: if any recipient is 'system', dispatch to system handler
    for (const r of results) {
        if (r.agent === 'system') {
            systemHandler.handleMessage(r.id, fromAgent, message, ch).catch(() => {});
            break;
        }
    }

    // Fire-and-forget: if this is a discussion message, trigger virtual agent processing
    if (discussionId && fromAgent !== 'system') {
        const { notifySystem } = require('./system-notify');
        notifySystem({ type: 'virtual-agent', discussionId, triggerType: 'message' }).catch(() => {});
    }

    // Fire-and-forget: trigger virtual agent responses for direct chat (no discussion)
    if (!discussionId && fromAgent !== 'system') {
        (async () => {
            try {
                const senderRow = await pool.query('SELECT virtual FROM agents WHERE agent = $1', [fromAgent]);
                if (senderRow.rows[0] && senderRow.rows[0].virtual) return;
                const vr = await pool.query('SELECT agent FROM agents WHERE agent = ANY($1) AND virtual = true', [recipients]);
                if (vr.rows.length === 0) return;
                const { handleDirectChat } = require('./virtual-agent');
                for (const row of vr.rows) {
                    // Find the message ID for this virtual agent recipient
                    const msgRow = results.find(r => r.agent === row.agent);
                    handleDirectChat(row.agent, fromAgent, message, msgRow ? msgRow.id : null).catch(() => {});
                }
            } catch (e) { /* ignore */ }
        })();
    }

    return { from_agent: fromAgent, to_agents: results, sent_at: results[0] ? results[0].sent_at : null };
}

async function chatReceive(agent, channel, afterId, fromAgent) {
    if (!agent) {
        throw Object.assign(new Error('Required field: agent'), { statusCode: 400 });
    }

    const ch = validateChannel(channel);

    let query = 'SELECT id, from_agent, to_agent, message, sent_at FROM chat_messages WHERE to_agent = $1 AND acked_at IS NULL';
    const params = [agent];

    if (ch === null) {
        query += ' AND channel IS NULL';
    } else {
        params.push(ch);
        query += ` AND channel = $${params.length}`;
    }

    if (fromAgent) {
        params.push(fromAgent);
        query += ` AND from_agent = $${params.length}`;
    }

    if (afterId !== undefined) {
        params.push(afterId);
        query += ` AND id > $${params.length}`;
    }

    query += ' ORDER BY id ASC';

    const result = await pool.query(query, params);

    logChat('receive', { agent, channel: ch, after_id: afterId || null, pending_count: result.rows.length, message_ids: result.rows.map(r => r.id) });

    return { messages: result.rows, pending_count: result.rows.length };
}

async function chatAck(agent, messageIds) {
    if (!agent || !messageIds || !Array.isArray(messageIds) || messageIds.length === 0) {
        throw Object.assign(new Error('Required fields: agent, message_ids (non-empty array)'), { statusCode: 400 });
    }

    const result = await pool.query(
        'UPDATE chat_messages SET acked_at = NOW() WHERE id = ANY($1) AND to_agent = $2 AND acked_at IS NULL RETURNING id',
        [messageIds, agent]
    );

    logChat('ack', { agent, requested_ids: messageIds, acked_ids: result.rows.map(r => r.id) });

    return { agent, acked: result.rows.length, acked_ids: result.rows.map(r => r.id) };
}

async function chatStatus(agent, channel) {
    if (!agent) {
        throw Object.assign(new Error('Required field: agent'), { statusCode: 400 });
    }

    const ch = validateChannel(channel);

    let channelFilter;
    let params;
    if (ch === null) {
        channelFilter = 'AND channel IS NULL';
        params = [agent];
    } else {
        channelFilter = 'AND channel = $2';
        params = [agent, ch];
    }

    const pending = await pool.query(
        `SELECT COUNT(*) as count FROM chat_messages WHERE to_agent = $1 AND acked_at IS NULL ${channelFilter}`,
        params
    );
    const latest = await pool.query(
        `SELECT MAX(id) as max_id FROM chat_messages WHERE to_agent = $1 ${channelFilter}`,
        params
    );
    const lastActivity = await pool.query(
        `SELECT MAX(sent_at) as last_sent FROM chat_messages WHERE to_agent = $1 ${channelFilter}`,
        params
    );
    const lastAcked = await pool.query(
        `SELECT MAX(acked_at) as last_acked FROM chat_messages WHERE to_agent = $1 AND acked_at IS NOT NULL ${channelFilter}`,
        params
    );

    logChat('status', { agent, channel: ch });

    return {
        agent,
        pending_count: parseInt(pending.rows[0].count),
        max_message_id: latest.rows[0].max_id,
        last_message_at: lastActivity.rows[0].last_sent,
        last_ack_at: lastAcked.rows[0].last_acked
    };
}

module.exports = { chatSend, chatReceive, chatAck, chatStatus };
