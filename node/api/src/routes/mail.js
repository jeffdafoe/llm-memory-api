const { Router } = require('express');
const pool = require('../db');
const { log } = require('../services/logger');

const router = Router();

function logMail(action, details) {
    log('mail', action, details);
}

router.post('/mail/send', async (req, res) => {
    try {
        const { to_agent, from_agent, subject, body } = req.body;

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
        const { agent } = req.body;

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

router.post('/mail/ack', async (req, res) => {
    try {
        const { agent, message_ids } = req.body;

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
