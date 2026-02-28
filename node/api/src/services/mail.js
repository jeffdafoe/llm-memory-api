// Service layer for mail operations (send, receive, ack).
// Extracted from routes/mail.js so both REST routes and MCP handler can share the same logic.

const pool = require('../db');
const { log } = require('./logger');

function logMail(action, details) {
    log('mail', action, details);
}

async function mailSend(toAgent, fromAgent, subject, body) {
    if (!toAgent || !fromAgent || !subject || !body) {
        throw Object.assign(new Error('Required fields: to_agent, from_agent, subject, body'), { statusCode: 400 });
    }

    const exists = await pool.query('SELECT 1 FROM agents WHERE agent = $1', [toAgent]);
    if (exists.rows.length === 0) {
        throw Object.assign(new Error(`Agent "${toAgent}" is not registered`), { statusCode: 404 });
    }

    const result = await pool.query(
        'INSERT INTO mail (to_agent, from_agent, subject, body) VALUES ($1, $2, $3, $4) RETURNING id, sent_at',
        [toAgent, fromAgent, subject, body]
    );

    logMail('send', { from_agent: fromAgent, to_agent: toAgent, mail_id: result.rows[0].id, subject });

    return {
        id: result.rows[0].id,
        to_agent: toAgent,
        from_agent: fromAgent,
        subject,
        sent_at: result.rows[0].sent_at
    };
}

async function mailReceive(agent) {
    if (!agent) {
        throw Object.assign(new Error('Required field: agent'), { statusCode: 400 });
    }

    const result = await pool.query(
        'SELECT id, from_agent, to_agent, subject, body, sent_at FROM mail WHERE to_agent = $1 AND acked_at IS NULL ORDER BY sent_at ASC',
        [agent]
    );

    logMail('receive', { agent, pending_count: result.rows.length, message_ids: result.rows.map(r => r.id) });

    return { messages: result.rows };
}

async function mailAck(agent, messageIds) {
    if (!agent || !messageIds || !Array.isArray(messageIds) || messageIds.length === 0) {
        throw Object.assign(new Error('Required fields: agent, message_ids (array of UUIDs)'), { statusCode: 400 });
    }

    const result = await pool.query(
        'UPDATE mail SET acked_at = NOW() WHERE id = ANY($1) AND to_agent = $2 AND acked_at IS NULL RETURNING id',
        [messageIds, agent]
    );

    logMail('ack', { agent, requested_ids: messageIds, acked_ids: result.rows.map(r => r.id) });

    return { agent, acked: result.rows.length, acked_ids: result.rows.map(r => r.id) };
}

module.exports = { mailSend, mailReceive, mailAck };
