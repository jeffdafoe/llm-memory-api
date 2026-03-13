// Service layer for mail operations (send, receive, ack).
// Extracted from routes/mail.js so both REST routes and MCP handler can share the same logic.

const pool = require('../db');
const { log } = require('./logger');
const { requireByName } = require('./actors');

function logMail(action, details) {
    log('mail', action, details);
}

async function mailSend(toAgent, fromAgent, subject, body) {
    if (!toAgent || !fromAgent || !subject || !body) {
        throw Object.assign(new Error('Required fields: to_agent, from_agent, subject, body'), { statusCode: 400 });
    }

    const toActor = await requireByName(toAgent);
    const fromActor = await requireByName(fromAgent);

    const result = await pool.query(
        'INSERT INTO mail (to_actor_id, from_actor_id, subject, body) VALUES ($1, $2, $3, $4) RETURNING id, sent_at',
        [toActor.id, fromActor.id, subject, body]
    );

    logMail('send', { from_agent: fromAgent, to_agent: toAgent, mail_id: result.rows[0].id, subject });

    // Fire-and-forget: trigger virtual agent response for direct mail
    if (fromAgent !== 'system') {
        (async () => {
            try {
                const recipientRow = await pool.query('SELECT virtual FROM agent_configuration WHERE actor_id = $1', [toActor.id]);
                if (!recipientRow.rows[0] || !recipientRow.rows[0].virtual) return;
                const senderRow = await pool.query('SELECT virtual FROM agent_configuration WHERE actor_id = $1', [fromActor.id]);
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

    const actor = await requireByName(agent);

    const result = await pool.query(
        `SELECT m.id, fa.name AS from_agent, ta.name AS to_agent, m.subject, m.body, m.sent_at
         FROM mail m
         JOIN actors fa ON fa.id = m.from_actor_id
         JOIN actors ta ON ta.id = m.to_actor_id
         WHERE m.to_actor_id = $1 AND m.acked_at IS NULL AND m.deleted_at IS NULL
         ORDER BY m.sent_at ASC`,
        [actor.id]
    );

    logMail('receive', { agent, pending_count: result.rows.length, message_ids: result.rows.map(r => r.id) });

    return { messages: result.rows };
}

async function mailAck(agent, messageIds) {
    if (!agent || !messageIds || !Array.isArray(messageIds) || messageIds.length === 0) {
        throw Object.assign(new Error('Required fields: agent, message_ids (array of UUIDs)'), { statusCode: 400 });
    }

    const actor = await requireByName(agent);

    const result = await pool.query(
        'UPDATE mail SET acked_at = NOW() WHERE id = ANY($1) AND to_actor_id = $2 AND acked_at IS NULL RETURNING id',
        [messageIds, actor.id]
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

    const fromActor = await requireByName(fromAgent);

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

    values.push(id, fromActor.id);

    const result = await pool.query(
        `UPDATE mail SET ${setClauses.join(', ')}
         WHERE id = $${paramIndex++} AND from_actor_id = $${paramIndex++} AND acked_at IS NULL AND deleted_at IS NULL
         RETURNING id, subject, sent_at`,
        values
    );

    if (result.rows.length === 0) {
        throw Object.assign(new Error('Mail not found, not owned by you, or already acked'), { statusCode: 404 });
    }

    logMail('edit', { from_agent: fromAgent, mail_id: id, fields: [subject ? 'subject' : null, body ? 'body' : null].filter(Boolean) });

    // Return with to_agent name resolved
    const mailRow = await pool.query(
        `SELECT ta.name AS to_agent FROM mail m JOIN actors ta ON ta.id = m.to_actor_id WHERE m.id = $1`,
        [id]
    );

    return { id: result.rows[0].id, to_agent: mailRow.rows[0].to_agent, subject: result.rows[0].subject, sent_at: result.rows[0].sent_at };
}

// Unsend (soft delete) an unacked mail message (sender only)
async function mailUnsend(id, fromAgent) {
    if (!id || !fromAgent) {
        throw Object.assign(new Error('Required fields: id, from_agent'), { statusCode: 400 });
    }

    const fromActor = await requireByName(fromAgent);

    const result = await pool.query(
        `UPDATE mail SET deleted_at = NOW()
         WHERE id = $1 AND from_actor_id = $2 AND acked_at IS NULL AND deleted_at IS NULL
         RETURNING id, subject`,
        [id, fromActor.id]
    );

    if (result.rows.length === 0) {
        throw Object.assign(new Error('Mail not found, not owned by you, or already acked'), { statusCode: 404 });
    }

    // Resolve to_agent name
    const mailRow = await pool.query(
        `SELECT ta.name AS to_agent FROM mail m JOIN actors ta ON ta.id = m.to_actor_id WHERE m.id = $1`,
        [id]
    );

    logMail('unsend', { from_agent: fromAgent, mail_id: id, to_agent: mailRow.rows[0].to_agent, subject: result.rows[0].subject });

    return { id: result.rows[0].id, to_agent: mailRow.rows[0].to_agent, subject: result.rows[0].subject };
}

module.exports = { mailSend, mailReceive, mailAck, mailEdit, mailUnsend };
