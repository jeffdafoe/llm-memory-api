const pool = require('../db');

const SYSTEM_AGENT = 'system';

async function sendSystemMessage(toAgent, message, channel) {
    const result = await pool.query(
        'INSERT INTO chat_messages (from_agent, to_agent, message, channel) VALUES ($1, $2, $3, $4) RETURNING id, sent_at',
        [SYSTEM_AGENT, toAgent, message, channel || null]
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

async function notifyDiscussionInvite(discussionId, topic, createdBy, invitedAgents) {
    const message = `You've been invited to discussion #${discussionId}: "${topic}" (created by ${createdBy})`;
    return sendSystemMessageToMany(invitedAgents, message, null);
}

module.exports = {
    sendSystemMessage,
    sendSystemMessageToMany,
    notifyDiscussionInvite,
};
