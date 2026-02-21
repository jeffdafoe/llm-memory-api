const { Router } = require('express');
const pool = require('../db');

const router = Router();

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

        res.json({
            id: result.rows[0].id,
            to_agent,
            from_agent,
            subject,
            sent_at: result.rows[0].sent_at
        });
    } catch (err) {
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/mail/check', async (req, res) => {
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

        res.json({ messages: result.rows });
    } catch (err) {
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/mail/ack', async (req, res) => {
    try {
        const { ids } = req.body;

        if (!ids || !Array.isArray(ids) || ids.length === 0) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required field: ids (array of UUIDs)' }
            });
        }

        const result = await pool.query(
            'UPDATE mail SET acked_at = NOW() WHERE id = ANY($1) AND acked_at IS NULL',
            [ids]
        );

        res.json({ acked: result.rowCount });
    } catch (err) {
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
