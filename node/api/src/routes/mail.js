const { Router } = require('express');
const pool = require('../db');

const router = Router();

router.post('/mail/send', async (req, res) => {
    const { to_namespace, from_namespace, subject, body } = req.body;

    if (!to_namespace || !from_namespace || !subject || !body) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: to_namespace, from_namespace, subject, body' }
        });
    }

    const result = await pool.query(
        'INSERT INTO mail (to_namespace, from_namespace, subject, body) VALUES ($1, $2, $3, $4) RETURNING id, sent_at',
        [to_namespace, from_namespace, subject, body]
    );

    res.json({
        id: result.rows[0].id,
        to_namespace,
        from_namespace,
        subject,
        sent_at: result.rows[0].sent_at
    });
});

router.post('/mail/check', async (req, res) => {
    const { namespace } = req.body;

    if (!namespace) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: namespace' }
        });
    }

    const result = await pool.query(
        'SELECT id, from_namespace, to_namespace, subject, body, sent_at FROM mail WHERE to_namespace = $1 AND acked_at IS NULL ORDER BY sent_at ASC',
        [namespace]
    );

    res.json({ messages: result.rows });
});

router.post('/mail/ack', async (req, res) => {
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
});

module.exports = router;
