const pool = require('../db');
const systemHandler = require('./system-handler');
const { requireByName } = require('./actors');

// Insert a text row and a single delivery row for a system message to one agent
async function sendSystemMessage(toAgent, message, discussionId) {
    const systemActor = await requireByName('system');
    const toActor = await requireByName(toAgent);

    const textResult = await pool.query(
        'INSERT INTO chat_message_texts (message, from_actor_id, discussion_id, sent_at) VALUES ($1, $2, $3, NOW()) RETURNING id, sent_at',
        [message, systemActor.id, discussionId || null]
    );
    const result = await pool.query(
        'INSERT INTO chat_messages (message_text_id, to_actor_id) VALUES ($1, $2) RETURNING id',
        [textResult.rows[0].id, toActor.id]
    );
    return { id: result.rows[0].id, sent_at: textResult.rows[0].sent_at };
}

async function sendSystemMessageToMany(toAgents, message, discussionId) {
    const results = [];
    for (const agent of toAgents) {
        const row = await sendSystemMessage(agent, message, discussionId);
        results.push({ agent, id: row.id, sent_at: row.sent_at });
    }
    return results;
}

// Post a single system event to a discussion.
// Uses system actor as both sender and receiver — these are broadcast events
// that appear in admin UI but are ignored by transports (which filter by
// to_actor_id=self and skip system-to-system messages).
async function sendDiscussionEvent(discussionId, message) {
    const systemActor = await requireByName('system');

    const textResult = await pool.query(
        'INSERT INTO chat_message_texts (message, from_actor_id, discussion_id, sent_at) VALUES ($1, $2, $3, NOW()) RETURNING id, sent_at',
        [message, systemActor.id, discussionId || null]
    );
    const result = await pool.query(
        'INSERT INTO chat_messages (message_text_id, to_actor_id) VALUES ($1, $2) RETURNING id',
        [textResult.rows[0].id, systemActor.id]
    );
    return { id: result.rows[0].id, sent_at: textResult.rows[0].sent_at };
}

async function notifyDiscussionInvite(discussionId, topic, createdBy, invitedAgents) {
    const message = `You've been invited to discussion #${discussionId}: "${topic}" (created by ${createdBy})`;
    return sendSystemMessageToMany(invitedAgents, message, null);
}

// Send a JSON message TO system (reverse direction — triggers system handler).
// Used by discussion service to trigger virtual agent processing.
async function notifySystem(payload) {
    const message = JSON.stringify(payload);
    const systemActor = await requireByName('system');

    const textResult = await pool.query(
        'INSERT INTO chat_message_texts (message, from_actor_id, discussion_id, sent_at) VALUES ($1, $2, $3, NOW()) RETURNING id, sent_at',
        [message, systemActor.id, null]
    );
    const result = await pool.query(
        'INSERT INTO chat_messages (message_text_id, to_actor_id) VALUES ($1, $2) RETURNING id',
        [textResult.rows[0].id, systemActor.id]
    );
    // Dispatch to system handler fire-and-forget
    systemHandler.handleMessage(result.rows[0].id, 'system', message, null).catch(() => {});
    return { id: result.rows[0].id, sent_at: textResult.rows[0].sent_at };
}

module.exports = {
    sendSystemMessage,
    sendSystemMessageToMany,
    sendDiscussionEvent,
    notifyDiscussionInvite,
    notifySystem,
};
