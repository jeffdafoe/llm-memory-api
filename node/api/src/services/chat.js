// Service layer for chat operations (send, receive, ack, status).
// Extracted from routes/chat.js so both REST routes and MCP handler can share the same logic.

const pool = require('../db');
const { log } = require('./logger');
const { broadcast } = require('./events');
const systemHandler = require('./system-handler');
const { resolveByName, resolveMultipleByName, requireByName, canAccessVirtualAgent } = require('./actors');

function logChat(action, details) {
    log('chat', action, details);
}

// Parse discussion_id from either an integer or a legacy 'discussion-{N}' channel string.
// Returns null if neither is provided.
function resolveDiscussionId(discussionId, channel) {
    if (discussionId) {
        const parsed = parseInt(discussionId, 10);
        if (isNaN(parsed)) return null;
        return parsed;
    }
    // Legacy backward compat: extract from channel string
    if (channel) {
        const match = channel.match(/^discussion-(\d+)$/);
        if (match) return parseInt(match[1], 10);
    }
    return null;
}

// Tool-use fields (toolCalls / toolCallId / toolsOffered, all optional) are
// MEM-119 additions for the Salem engine ↔ NPC chat path. Existing callers
// pass nothing and behavior is unchanged. When the message carries
// toolsOffered, handleDirectChat takes the tool-use branch. Returns
// `pendingReplyPromise` (or null) so wait-mode HTTP routes can await the
// VA's reply inline; non-wait callers ignore it and the dispatch stays
// fire-and-forget.
//
// sceneId (MEM-121) is the engine's per-cascade UUID. When present, it
// rides through to the chat row, the call log, and the VA's reply chat
// row so the admin UI can group all rows from one tavern conversation
// (or whatever scene) together. NULL for companion-mode.
async function chatSend(fromAgent, toAgents, discussionId, message, opts) {
    if (!fromAgent || message === undefined || message === null) {
        throw Object.assign(new Error('Required fields: from_agent, message'), { statusCode: 400 });
    }
    if (!toAgents && !discussionId) {
        throw Object.assign(new Error('Required: to_agents (array) or discussion_id'), { statusCode: 400 });
    }
    const toolCalls = opts && opts.toolCalls !== undefined ? opts.toolCalls : null;
    const toolCallId = opts && opts.toolCallId !== undefined ? opts.toolCallId : null;
    const toolsOffered = opts && opts.toolsOffered !== undefined ? opts.toolsOffered : null;
    const sceneId = opts && opts.sceneId !== undefined ? opts.sceneId : null;

    // Resolve sender
    const fromActor = await requireByName(fromAgent);

    const discussionParticipants = new Set();
    const recipientSet = new Set();

    if (discussionId) {
        const participants = await pool.query(
            `SELECT ac.name AS agent FROM discussion_participants dp
             JOIN actors ac ON ac.id = dp.actor_id
             WHERE dp.discussion_id = $1 AND dp.status = $2 AND dp.actor_id != $3`,
            [discussionId, 'joined', fromActor.id]
        );
        for (const row of participants.rows) {
            discussionParticipants.add(row.agent);
            recipientSet.add(row.agent);
        }
    }

    if (toAgents && toAgents.length === 1 && toAgents[0] === '*') {
        const known = await pool.query(
            `SELECT ac.name AS agent FROM agent_configuration agc
             JOIN actors ac ON ac.id = agc.actor_id
             WHERE ac.name != $1`,
            [fromAgent]
        );
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

    // Resolve all recipients to actor IDs (also validates they exist)
    const recipientActors = await resolveMultipleByName(recipients);
    for (const name of recipients) {
        if (!recipientActors.has(name)) {
            throw Object.assign(new Error(`Agent "${name}" is not registered`), { statusCode: 404 });
        }
    }

    // Insert one message text row
    const textResult = await pool.query(
        `INSERT INTO chat_message_texts
            (message, from_actor_id, discussion_id, sent_at, tool_calls, tool_call_id, tools_offered, scene_id)
         VALUES ($1, $2, $3, NOW(), $4, $5, $6, $7)
         RETURNING id, sent_at`,
        [
            message,
            fromActor.id,
            discussionId || null,
            toolCalls !== null ? JSON.stringify(toolCalls) : null,
            toolCallId,
            toolsOffered !== null ? JSON.stringify(toolsOffered) : null,
            sceneId,
        ]
    );
    const messageTextId = textResult.rows[0].id;
    const sentAt = textResult.rows[0].sent_at;

    // Insert one delivery row per recipient
    const results = [];
    for (const recipient of recipients) {
        const toActor = recipientActors.get(recipient);
        const result = await pool.query(
            'INSERT INTO chat_messages (message_text_id, to_actor_id) VALUES ($1, $2) RETURNING id',
            [messageTextId, toActor.id]
        );
        results.push({ id: result.rows[0].id, agent: recipient, sent_at: sentAt });
    }

    // Broadcast to admin WebSocket clients for live updates (once per logical message)
    if (results.length > 0) {
        broadcast('chat_message', {
            id: results[0].id,
            message_text_id: messageTextId,
            from_agent: fromAgent,
            to_agents: results.map(function(r) { return r.agent; }),
            message: message,
            discussion_id: discussionId || null,
            scene_id: sceneId,
            sent_at: sentAt
        });
    }

    logChat('send', { from_agent: fromAgent, to_agents: recipients, message_text_id: messageTextId, delivery_ids: results.map(r => r.id), discussion_id: discussionId || null });

    // Fire-and-forget: if any recipient is 'system', dispatch to system handler
    for (const r of results) {
        if (r.agent === 'system') {
            systemHandler.handleMessage(r.id, fromAgent, message, null).catch(() => {});
            break;
        }
    }

    // Fire-and-forget: if this is a discussion message, trigger virtual agent processing
    if (discussionId && fromAgent !== 'system') {
        const { notifySystem } = require('./system-notify');
        notifySystem({ type: 'virtual-agent', discussionId, triggerType: 'message' }).catch(() => {});
    }

    // VA eligibility check is hoisted out of the prior fire-and-forget IIFE
    // so wait-mode callers can know whether a reply is coming. Fast — two
    // small queries — and the same work the IIFE was doing async anyway.
    let pendingReplyPromise = null;
    if (!discussionId && fromAgent !== 'system') {
        try {
            const senderRow = await pool.query(
                'SELECT agc.virtual FROM agent_configuration agc WHERE agc.actor_id = $1',
                [fromActor.id]
            );
            const senderIsVirtual = senderRow.rows[0] && senderRow.rows[0].virtual;
            if (!senderIsVirtual) {
                const recipientIds = recipients.map(r => recipientActors.get(r).id);
                const vr = await pool.query(
                    `SELECT ac.name AS agent FROM agent_configuration agc
                     JOIN actors ac ON ac.id = agc.actor_id
                     WHERE agc.actor_id = ANY($1) AND agc.virtual = true`,
                    [recipientIds]
                );
                if (vr.rows.length > 0) {
                    const { handleDirectChat } = require('./virtual-agent');
                    const dispatches = [];
                    for (const row of vr.rows) {
                        const vrActor = recipientActors.get(row.agent);
                        if (vrActor) {
                            const hasAccess = await canAccessVirtualAgent(fromActor.id, vrActor.id);
                            if (!hasAccess) continue;
                        }
                        const msgRow = results.find(r => r.agent === row.agent);
                        dispatches.push({ agent: row.agent, msgId: msgRow ? msgRow.id : null });
                    }
                    // Single-recipient: expose the promise so wait-mode can
                    // await it. Multi-recipient: no clean way to surface
                    // multiple replies in one HTTP response; stay
                    // fire-and-forget (and leave wait-mode to reject).
                    if (dispatches.length === 1) {
                        const d = dispatches[0];
                        pendingReplyPromise = handleDirectChat(d.agent, fromAgent, message, d.msgId, {
                            toolsOffered, toolCallId, sceneId,
                        });
                        // Swallow rejection for non-wait callers so unhandled-promise
                        // warnings don't appear; wait-mode awaiters get the error.
                        pendingReplyPromise.catch(() => {});
                    } else {
                        for (const d of dispatches) {
                            handleDirectChat(d.agent, fromAgent, message, d.msgId, {
                                toolsOffered, toolCallId, sceneId,
                            }).catch(() => {});
                        }
                    }
                }
            }
        } catch (e) { /* ignore — eligibility check is best-effort */ }
    }

    return { from_agent: fromAgent, to_agents: results, sent_at: sentAt, pendingReplyPromise };
}

async function chatReceive(agent, discussionId, afterId, fromAgent) {
    if (!agent) {
        throw Object.assign(new Error('Required field: agent'), { statusCode: 400 });
    }

    const actor = await requireByName(agent);

    let query = `SELECT cm.id, fa.name AS from_agent, ta.name AS to_agent, cmt.message, cmt.sent_at,
                        cmt.tool_calls, cmt.tool_call_id, cmt.tools_offered
                 FROM chat_messages cm
                 JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
                 JOIN actors fa ON fa.id = cmt.from_actor_id
                 JOIN actors ta ON ta.id = cm.to_actor_id
                 WHERE cm.to_actor_id = $1 AND cm.acked_at IS NULL AND cm.deleted_at IS NULL`;
    const params = [actor.id];

    if (discussionId) {
        params.push(discussionId);
        query += ` AND cmt.discussion_id = $${params.length}`;
    } else {
        query += ' AND cmt.discussion_id IS NULL';
    }

    if (fromAgent) {
        const fromActor = await requireByName(fromAgent);
        params.push(fromActor.id);
        query += ` AND cmt.from_actor_id = $${params.length}`;
    }

    if (afterId !== undefined) {
        params.push(afterId);
        query += ` AND cm.id > $${params.length}`;
    }

    query += ' ORDER BY cm.id ASC';

    const result = await pool.query(query, params);

    logChat('receive', { agent, discussion_id: discussionId || null, after_id: afterId || null, pending_count: result.rows.length, message_ids: result.rows.map(r => r.id) });

    return { messages: result.rows, pending_count: result.rows.length };
}

async function chatAck(agent, messageIds) {
    if (!agent || !messageIds || !Array.isArray(messageIds) || messageIds.length === 0) {
        throw Object.assign(new Error('Required fields: agent, message_ids (non-empty array)'), { statusCode: 400 });
    }

    const actor = await requireByName(agent);

    const result = await pool.query(
        'UPDATE chat_messages SET acked_at = NOW() WHERE id = ANY($1) AND to_actor_id = $2 AND acked_at IS NULL RETURNING id',
        [messageIds, actor.id]
    );

    logChat('ack', { agent, requested_ids: messageIds, acked_ids: result.rows.map(r => r.id) });

    return { agent, acked: result.rows.length, acked_ids: result.rows.map(r => r.id) };
}

async function chatStatus(agent, discussionId) {
    if (!agent) {
        throw Object.assign(new Error('Required field: agent'), { statusCode: 400 });
    }

    const actor = await requireByName(agent);

    let discussionFilter;
    let params;
    if (discussionId) {
        discussionFilter = 'AND cmt.discussion_id = $2';
        params = [actor.id, discussionId];
    } else {
        discussionFilter = 'AND cmt.discussion_id IS NULL';
        params = [actor.id];
    }

    const pending = await pool.query(
        `SELECT COUNT(*) as count FROM chat_messages cm
         JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
         WHERE cm.to_actor_id = $1 AND cm.acked_at IS NULL AND cm.deleted_at IS NULL ${discussionFilter}`,
        params
    );
    const latest = await pool.query(
        `SELECT MAX(cm.id) as max_id FROM chat_messages cm
         JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
         WHERE cm.to_actor_id = $1 AND cm.deleted_at IS NULL ${discussionFilter}`,
        params
    );
    const lastActivity = await pool.query(
        `SELECT MAX(cmt.sent_at) as last_sent FROM chat_messages cm
         JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
         WHERE cm.to_actor_id = $1 AND cm.deleted_at IS NULL ${discussionFilter}`,
        params
    );
    const lastAcked = await pool.query(
        `SELECT MAX(cm.acked_at) as last_acked FROM chat_messages cm
         JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
         WHERE cm.to_actor_id = $1 AND cm.acked_at IS NOT NULL AND cm.deleted_at IS NULL ${discussionFilter}`,
        params
    );

    logChat('status', { agent, discussion_id: discussionId || null });

    return {
        agent,
        pending_count: parseInt(pending.rows[0].count),
        max_message_id: latest.rows[0].max_id,
        last_message_at: lastActivity.rows[0].last_sent,
        last_ack_at: lastAcked.rows[0].last_acked
    };
}

module.exports = { chatSend, chatReceive, chatAck, chatStatus, resolveDiscussionId };
