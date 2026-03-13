const pool = require('../db');
const systemHandler = require('./system-handler');
const { requireByName } = require('./actors');

async function sendSystemMessage(toAgent, message, channel) {
    const systemActor = await requireByName('system');
    const toActor = await requireByName(toAgent);
    const result = await pool.query(
        'INSERT INTO chat_messages (from_actor_id, to_actor_id, message, channel) VALUES ($1, $2, $3, $4) RETURNING id, sent_at',
        [systemActor.id, toActor.id, message, channel || null]
    );
    return result.rows[0];
}

async function sendSystemMessageToMany(toAgents, message, channel) {
    const results = [];
    for (const agent of toAgents) {
        const row = await sendSystemMessage(agent, message, channel);
        results.push({ agent, id: row.id, sent_at: row.sent_at });
    }
    return results;
}

// Post a single system event to a discussion channel.
// Uses system actor as both sender and receiver — these are broadcast events
// that appear in admin UI but are ignored by transports (which filter by
// to_actor_id=self and skip system-to-system messages).
async function sendDiscussionEvent(channel, message) {
    const systemActor = await requireByName('system');
    const result = await pool.query(
        'INSERT INTO chat_messages (from_actor_id, to_actor_id, message, channel) VALUES ($1, $2, $3, $4) RETURNING id, sent_at',
        [systemActor.id, systemActor.id, message, channel]
    );
    return result.rows[0];
}

async function notifyDiscussionInvite(discussionId, topic, createdBy, invitedAgents) {
    const message = `You've been invited to discussion #${discussionId}: "${topic}" (created by ${createdBy})`;
    return sendSystemMessageToMany(invitedAgents, message, null);
}

// Send a JSON message TO system (reverse direction — triggers system handler).
// Used by discussion service to trigger virtual agent processing.
async function notifySystem(payload, channel) {
    const message = JSON.stringify(payload);
    const systemActor = await requireByName('system');
    const result = await pool.query(
        'INSERT INTO chat_messages (from_actor_id, to_actor_id, message, channel) VALUES ($1, $2, $3, $4) RETURNING id, sent_at',
        [systemActor.id, systemActor.id, message, channel || null]
    );
    // Dispatch to system handler fire-and-forget
    systemHandler.handleMessage(result.rows[0].id, 'system', message, channel || null).catch(() => {});
    return result.rows[0];
}

module.exports = {
    sendSystemMessage,
    sendSystemMessageToMany,
    sendDiscussionEvent,
    notifyDiscussionInvite,
    notifySystem,
};
