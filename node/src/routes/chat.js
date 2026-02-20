const { Router } = require('express');
const pool = require('../db');

const router = Router();

router.post('/chat/send', async (req, res) => {
    const { channel, from_namespace, message } = req.body;

    if (!channel || !from_namespace || !message) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: channel, from_namespace, message' }
        });
    }

    const result = await pool.query(
        'INSERT INTO chat_messages (channel, from_namespace, message) VALUES ($1, $2, $3) RETURNING id, sent_at',
        [channel, from_namespace, message]
    );

    res.json({
        id: result.rows[0].id,
        channel,
        from_namespace,
        sent_at: result.rows[0].sent_at
    });
});

router.post('/chat/receive', async (req, res) => {
    const { channel, since_id } = req.body;

    if (!channel) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: channel' }
        });
    }

    const sinceId = since_id || 0;

    const result = await pool.query(
        'SELECT id, channel, from_namespace, message, sent_at FROM chat_messages WHERE channel = $1 AND id > $2 ORDER BY id ASC',
        [channel, sinceId]
    );

    res.json({
        messages: result.rows
    });
});

module.exports = router;
