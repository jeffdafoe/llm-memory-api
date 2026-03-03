const { Router } = require('express');
const pool = require('../db');
const { log } = require('../services/logger');

const router = Router();

function logMail(action, details) {
    log('mail', action, details);
}

router.post('/mail/send', async (req, res) => {
    try {
        let { to_agent, from_agent, subject, body } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (from_agent && from_agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'from_agent does not match authenticated agent' } });
            }
            from_agent = req.authenticatedAgent;
        }

        if (!to_agent || !from_agent || !subject || !body) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: to_agent, from_agent, subject, body' }
            });
        }

        const exists = await pool.query('SELECT 1 FROM agents WHERE agent = $1', [to_agent]);
        if (exists.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: `Agent "${to_agent}" is not registered` }
            });
        }

        const result = await pool.query(
            'INSERT INTO mail (to_agent, from_agent, subject, body) VALUES ($1, $2, $3, $4) RETURNING id, sent_at',
            [to_agent, from_agent, subject, body]
        );

        logMail('send', { from_agent, to_agent, mail_id: result.rows[0].id, subject });

        res.json({
            id: result.rows[0].id,
            to_agent,
            from_agent,
            subject,
            sent_at: result.rows[0].sent_at
        });
    } catch (err) {
        console.error('Mail send error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/mail/receive', async (req, res) => {
    try {
        let { agent } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (agent && agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
            }
            agent = req.authenticatedAgent;
        }

        if (!agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
            });
        }

        const result = await pool.query(
            'SELECT id, from_agent, to_agent, subject, body, sent_at FROM mail WHERE to_agent = $1 AND acked_at IS NULL ORDER BY sent_at ASC',
            [agent]
        );

        logMail('receive', { agent, pending_count: result.rows.length, message_ids: result.rows.map(r => r.id) });

        res.json({ messages: result.rows });
    } catch (err) {
        console.error('Mail receive error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/mail/edit', async (req, res) => {
    try {
        let { id, from_agent, subject, body } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (from_agent && from_agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'from_agent does not match authenticated agent' } });
            }
            from_agent = req.authenticatedAgent;
        }

        if (!id || !from_agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: id, from_agent. Optional: subject, body' }
            });
        }

        if (!subject && !body) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'At least one of subject or body must be provided' }
            });
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

        values.push(id, from_agent);

        const result = await pool.query(
            `UPDATE mail SET ${setClauses.join(', ')} WHERE id = $${paramIndex++} AND from_agent = $${paramIndex} AND acked_at IS NULL RETURNING id, to_agent, subject, sent_at`,
            values
        );

        if (result.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Mail not found, not owned by you, or already acked' }
            });
        }

        logMail('edit', { from_agent, mail_id: id, fields: [subject ? 'subject' : null, body ? 'body' : null].filter(Boolean) });

        res.json(result.rows[0]);
    } catch (err) {
        console.error('Mail edit error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/mail/ack', async (req, res) => {
    try {
        let { agent, message_ids } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (agent && agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
            }
            agent = req.authenticatedAgent;
        }

        if (!agent || !message_ids || !Array.isArray(message_ids) || message_ids.length === 0) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: agent, message_ids (array of UUIDs)' }
            });
        }

        const result = await pool.query(
            'UPDATE mail SET acked_at = NOW() WHERE id = ANY($1) AND to_agent = $2 AND acked_at IS NULL RETURNING id',
            [message_ids, agent]
        );

        logMail('ack', { agent, requested_ids: message_ids, acked_ids: result.rows.map(r => r.id) });

        res.json({
            agent,
            acked: result.rows.length,
            acked_ids: result.rows.map(r => r.id)
        });
    } catch (err) {
        console.error('Mail ack error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
