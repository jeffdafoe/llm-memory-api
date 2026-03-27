// Service layer for mail operations (send, receive, ack).
// Extracted from routes/mail.js so both REST routes and MCP handler can share the same logic.

const pool = require('../db');
const { log } = require('./logger');
const { requireByName, canAccessVirtualAgent } = require('./actors');

function logMail(action, details) {
    log('mail', action, details);
}

async function mailSend(toAgent, fromAgent, subject, body, inReplyTo) {
    if (!toAgent || !fromAgent || !subject || !body) {
        throw Object.assign(new Error('Required fields: to_agent, from_agent, subject, body'), { statusCode: 400 });
    }

    const toActor = await requireByName(toAgent);
    const fromActor = await requireByName(fromAgent);

    // Build insert with optional in_reply_to
    let sql, params;
    if (inReplyTo) {
        sql = 'INSERT INTO mail (to_actor_id, from_actor_id, subject, body, in_reply_to) VALUES ($1, $2, $3, $4, $5) RETURNING id, sent_at';
        params = [toActor.id, fromActor.id, subject, body, inReplyTo];
    } else {
        sql = 'INSERT INTO mail (to_actor_id, from_actor_id, subject, body) VALUES ($1, $2, $3, $4) RETURNING id, sent_at';
        params = [toActor.id, fromActor.id, subject, body];
    }

    const result = await pool.query(sql, params);

    logMail('send', { from_agent: fromAgent, to_agent: toAgent, mail_id: result.rows[0].id, subject });

    // Fire-and-forget: trigger virtual agent response for direct mail
    if (fromAgent !== 'system') {
        (async () => {
            try {
                // Parallel lookup: check if recipient is virtual and sender is not
                const [recipientRow, senderRow] = await Promise.all([
                    pool.query('SELECT virtual FROM agent_configuration WHERE actor_id = $1', [toActor.id]),
                    pool.query('SELECT virtual FROM agent_configuration WHERE actor_id = $1', [fromActor.id])
                ]);
                if (!recipientRow.rows[0] || !recipientRow.rows[0].virtual) return;
                if (senderRow.rows[0] && senderRow.rows[0].virtual) return;
                // Access control: check if sender can use this virtual agent
                const hasAccess = await canAccessVirtualAgent(fromActor.id, toActor.id);
                if (!hasAccess) {
                    logMail('virtual-agent-access-denied', { from_agent: fromAgent, to_agent: toAgent });
                    return;
                }
                const { handleDirectMail } = require('./virtual-agent');
                handleDirectMail(toAgent, fromAgent, result.rows[0].id).catch(err => {
                    logMail('virtual-agent-trigger-error', { to_agent: toAgent, from_agent: fromAgent, error: err.message });
                });
            } catch (e) {
                logMail('virtual-agent-trigger-error', { to_agent: toAgent, from_agent: fromAgent, error: e.message });
            }
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

async function mailReceive(agent, ids) {
    if (!agent) {
        throw Object.assign(new Error('Required field: agent'), { statusCode: 400 });
    }

    const actor = await requireByName(agent);

    let result;
    if (ids && Array.isArray(ids) && ids.length > 0) {
        // Validate UUIDs
        const uuidRegex = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
        for (const id of ids) {
            if (typeof id !== 'string' || !uuidRegex.test(id)) {
                throw Object.assign(new Error('Invalid UUID in ids array'), { statusCode: 400 });
            }
        }
        // Fetch specific messages by ID (must belong to this agent, unacked)
        result = await pool.query(
            `SELECT m.id, fa.name AS from_agent, ta.name AS to_agent, m.subject, m.body, m.sent_at, m.in_reply_to
             FROM mail m
             JOIN actors fa ON fa.id = m.from_actor_id
             JOIN actors ta ON ta.id = m.to_actor_id
             WHERE m.id = ANY($1) AND m.to_actor_id = $2 AND m.acked_at IS NULL AND m.deleted_at IS NULL
             ORDER BY m.sent_at ASC`,
            [ids, actor.id]
        );
    } else {
        throw Object.assign(new Error(
            'Required field: ids (array of mail UUIDs). ' +
            'Use mail_check first to list unread mail with IDs, ' +
            'then call mail_receive with specific IDs to read full content, ' +
            'then call mail_ack with those IDs after processing.'
        ), { statusCode: 400 });
    }

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

// Lightweight list of unread mail — subject, sender, date, ID, body preview. No full body.
async function mailCheck(agent) {
    if (!agent) {
        throw Object.assign(new Error('Required field: agent'), { statusCode: 400 });
    }

    const actor = await requireByName(agent);

    const result = await pool.query(
        `SELECT m.id, fa.name AS from_agent, m.subject, m.sent_at, m.in_reply_to,
                SUBSTRING(m.body FROM 1 FOR 200) AS body_preview
         FROM mail m
         JOIN actors fa ON fa.id = m.from_actor_id
         WHERE m.to_actor_id = $1 AND m.acked_at IS NULL AND m.deleted_at IS NULL
         ORDER BY m.sent_at ASC`,
        [actor.id]
    );

    logMail('check', { agent, pending_count: result.rows.length });

    return { messages: result.rows };
}

// List mail sent by this agent, newest first (body truncated to preview)
async function mailSent(agent, options = {}) {
    if (!agent) {
        throw Object.assign(new Error('Required field: agent'), { statusCode: 400 });
    }

    const actor = await requireByName(agent);
    const limit = Math.min(options.limit || 50, 100);
    const offset = options.offset || 0;

    // Build optional filters
    const conditions = ['m.from_actor_id = $1', 'm.deleted_at IS NULL'];
    const values = [actor.id];
    let paramIndex = 2;

    if (options.to) {
        const toActor = await requireByName(options.to);
        conditions.push(`m.to_actor_id = $${paramIndex++}`);
        values.push(toActor.id);
    }

    values.push(limit, offset);

    const result = await pool.query(
        `SELECT m.id, ta.name AS to_agent, m.subject,
                SUBSTRING(m.body FROM 1 FOR 200) AS body_preview,
                m.sent_at, m.acked_at, m.in_reply_to
         FROM mail m
         JOIN actors ta ON ta.id = m.to_actor_id
         WHERE ${conditions.join(' AND ')}
         ORDER BY m.sent_at DESC
         LIMIT $${paramIndex++} OFFSET $${paramIndex++}`,
        values
    );

    logMail('sent', { agent, count: result.rows.length });

    return { messages: result.rows };
}

// List acked (read) mail received by this agent, newest first
async function mailHistory(agent, options = {}) {
    if (!agent) {
        throw Object.assign(new Error('Required field: agent'), { statusCode: 400 });
    }

    const actor = await requireByName(agent);
    const limit = Math.min(options.limit || 50, 100);
    const offset = options.offset || 0;

    // Build optional filters
    const conditions = ['m.to_actor_id = $1', 'm.acked_at IS NOT NULL', 'm.deleted_at IS NULL'];
    const values = [actor.id];
    let paramIndex = 2;

    if (options.from) {
        const fromActor = await requireByName(options.from);
        conditions.push(`m.from_actor_id = $${paramIndex++}`);
        values.push(fromActor.id);
    }

    values.push(limit, offset);

    const result = await pool.query(
        `SELECT m.id, fa.name AS from_agent, m.subject, m.body, m.sent_at, m.acked_at, m.in_reply_to
         FROM mail m
         JOIN actors fa ON fa.id = m.from_actor_id
         WHERE ${conditions.join(' AND ')}
         ORDER BY m.sent_at DESC
         LIMIT $${paramIndex++} OFFSET $${paramIndex++}`,
        values
    );

    logMail('history', { agent, count: result.rows.length });

    return { messages: result.rows };
}

module.exports = { mailSend, mailReceive, mailCheck, mailAck, mailEdit, mailUnsend, mailSent, mailHistory };
