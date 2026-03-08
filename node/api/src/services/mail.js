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

    // Fire-and-forget: trigger virtual agent response for direct mail
    if (fromAgent !== 'system') {
        (async () => {
            try {
                const recipientRow = await pool.query('SELECT virtual FROM agents WHERE agent = $1', [toAgent]);
                if (!recipientRow.rows[0] || !recipientRow.rows[0].virtual) return;
                const senderRow = await pool.query('SELECT virtual FROM agents WHERE agent = $1', [fromAgent]);
                if (senderRow.rows[0] && senderRow.rows[0].virtual) return;
                const { handleDirectMail } = require('./virtual-agent');
                handleDirectMail(toAgent, fromAgent, result.rows[0].id).catch(() => {});
            } catch (e) { /* ignore */ }
        })();
    }

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

// Edit an unacked mail message (sender only, before recipient acks)
async function mailEdit(id, fromAgent, subject, body) {
    if (!id || !fromAgent) {
        throw Object.assign(new Error('Required fields: id, from_agent. Optional: subject, body'), { statusCode: 400 });
    }
    if (!subject && !body) {
        throw Object.assign(new Error('At least one of subject or body must be provided'), { statusCode: 400 });
    }

    // Build dynamic SET clause
    const setClauses = [];
    const values = [];
    let paramIndex = 1;

    if (subject) {
        setClauses.push(`subject = $${paramIndex++}`);
        values.push(subject);
    }
    if (body) {
        setClauses.push(`body = $${paramIndex++}`);
        values.push(body);
    }

    values.push(id, fromAgent);

    const result = await pool.query(
        `UPDATE mail SET ${setClauses.join(', ')} WHERE id = $${paramIndex++} AND from_agent = $${paramIndex++} AND acked_at IS NULL AND deleted_at IS NULL RETURNING id, to_agent, subject, sent_at`,
        values
    );

    if (result.rows.length === 0) {
        throw Object.assign(new Error('Mail not found, not owned by you, or already acked'), { statusCode: 404 });
    }

    logMail('edit', { from_agent: fromAgent, mail_id: id, fields: [subject ? 'subject' : null, body ? 'body' : null].filter(Boolean) });

    return result.rows[0];
}

// Unsend (soft delete) an unacked mail message (sender only)
async function mailUnsend(id, fromAgent) {
    if (!id || !fromAgent) {
        throw Object.assign(new Error('Required fields: id, from_agent'), { statusCode: 400 });
    }

    const result = await pool.query(
        'UPDATE mail SET deleted_at = NOW() WHERE id = $1 AND from_agent = $2 AND acked_at IS NULL AND deleted_at IS NULL RETURNING id, to_agent, subject',
        [id, fromAgent]
    );

    if (result.rows.length === 0) {
        throw Object.assign(new Error('Mail not found, not owned by you, or already acked'), { statusCode: 404 });
    }

    logMail('unsend', { from_agent: fromAgent, mail_id: id, to_agent: result.rows[0].to_agent, subject: result.rows[0].subject });

    return result.rows[0];
}

module.exports = { mailSend, mailReceive, mailAck, mailEdit, mailUnsend };
