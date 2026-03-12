const { Router } = require('express');
const pool = require('../db');
const { log, logError } = require('../services/logger');

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
        logError('mail', 'send', { agent: req.authenticatedAgent || req.body.from_agent, message: err.message, detail: err.stack });
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
            'SELECT id, from_agent, to_agent, subject, body, sent_at FROM mail WHERE to_agent = $1 AND acked_at IS NULL AND deleted_at IS NULL ORDER BY sent_at ASC',
            [agent]
        );

        logMail('receive', { agent, pending_count: result.rows.length, message_ids: result.rows.map(r => r.id) });

        res.json({ messages: result.rows });
    } catch (err) {
        logError('mail', 'receive', { agent: req.authenticatedAgent || req.body.agent, message: err.message, detail: err.stack });
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
            `UPDATE mail SET ${setClauses.join(', ')} WHERE id = $${paramIndex++} AND from_agent = $${paramIndex++} AND acked_at IS NULL AND deleted_at IS NULL RETURNING id, to_agent, subject, sent_at`,
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
        logError('mail', 'edit', { agent: req.authenticatedAgent || req.body.from_agent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

// Sender can unsend (delete) mail before recipient acks it
router.post('/mail/unsend', async (req, res) => {
    try {
        let { id, from_agent } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (from_agent && from_agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'from_agent does not match authenticated agent' } });
            }
            from_agent = req.authenticatedAgent;
        }

        if (!id || !from_agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: id, from_agent' }
            });
        }

        const result = await pool.query(
            'UPDATE mail SET deleted_at = NOW() WHERE id = $1 AND from_agent = $2 AND acked_at IS NULL AND deleted_at IS NULL RETURNING id, to_agent, subject',
            [id, from_agent]
        );

        if (result.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Mail not found, not owned by you, or already acked' }
            });
        }

        logMail('unsend', { from_agent, mail_id: id, to_agent: result.rows[0].to_agent, subject: result.rows[0].subject });

        res.json(result.rows[0]);
    } catch (err) {
        logError('mail', 'unsend', { agent: req.authenticatedAgent || req.body.from_agent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/mail/ack', async (req, res) => {
    try {
        let { agent, ids, message_ids } = req.body;

        // Accept both 'ids' (canonical) and 'message_ids' (legacy) — remove message_ids after 2026-04-15
        ids = ids || message_ids;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (agent && agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
            }
            agent = req.authenticatedAgent;
        }

        if (!agent || !ids || !Array.isArray(ids) || ids.length === 0) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: ids (non-empty array of UUIDs)' }
            });
        }

        const result = await pool.query(
            'UPDATE mail SET acked_at = NOW() WHERE id = ANY($1) AND to_agent = $2 AND acked_at IS NULL RETURNING id',
            [ids, agent]
        );

        logMail('ack', { agent, requested_ids: ids, acked_ids: result.rows.map(r => r.id) });

        res.json({
            agent,
            acked: result.rows.length,
            acked_ids: result.rows.map(r => r.id)
        });
    } catch (err) {
        logError('mail', 'ack', { agent: req.authenticatedAgent || req.body.agent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
