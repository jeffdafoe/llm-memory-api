const { Router } = require('express');
const pool = require('../db');

const router = Router();

function logChat(action, details) {
    const timestamp = new Date().toISOString();
    console.log(`[chat] ${timestamp} ${action}:`, JSON.stringify(details));
}

router.post('/chat/send', async (req, res) => {
    try {
        const { from_agent, to_agent, message } = req.body;

        if (!from_agent || !to_agent || !message) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: from_agent, to_agent, message' }
            });
        }

        if (to_agent === '*') {
            const known = await pool.query(
                'SELECT agent FROM agents WHERE agent != $1',
                [from_agent]
            );

            const recipients = known.rows.map(r => r.agent);

            if (recipients.length === 0) {
                return res.status(400).json({
                    error: { code: 'NO_RECIPIENTS', message: 'No registered agents to broadcast to' }
                });
            }

            const ids = [];
            for (const recipient of recipients) {
                const result = await pool.query(
                    'INSERT INTO chat_messages (from_agent, to_agent, message) VALUES ($1, $2, $3) RETURNING id, sent_at',
                    [from_agent, recipient, message]
                );
                ids.push({ id: result.rows[0].id, to_agent: recipient });
            }

            logChat('send_broadcast', { from_agent, recipients: ids.map(i => i.to_agent), message_ids: ids.map(i => i.id) });

            return res.json({
                broadcast: true,
                from_agent,
                recipients: ids,
                sent_at: ids[0] ? ids[0].sent_at : null
            });
        }

        const exists = await pool.query('SELECT 1 FROM agents WHERE agent = $1', [to_agent]);
        if (exists.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: `Agent "${to_agent}" is not registered` }
            });
        }

        const result = await pool.query(
            'INSERT INTO chat_messages (from_agent, to_agent, message) VALUES ($1, $2, $3) RETURNING id, sent_at',
            [from_agent, to_agent, message]
        );

        logChat('send', { from_agent, to_agent, message_id: result.rows[0].id });

        res.json({
            id: result.rows[0].id,
            from_agent,
            to_agent,
            sent_at: result.rows[0].sent_at
        });
    } catch (err) {
        console.error('Chat send error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/chat/receive', async (req, res) => {
    try {
        const { agent } = req.body;

        if (!agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
            });
        }

        const result = await pool.query(
            'SELECT id, from_agent, to_agent, message, sent_at FROM chat_messages WHERE to_agent = $1 AND acked_at IS NULL ORDER BY id ASC',
            [agent]
        );

        logChat('receive', { agent, pending_count: result.rows.length, message_ids: result.rows.map(r => r.id) });

        res.json({
            messages: result.rows,
            pending_count: result.rows.length
        });
    } catch (err) {
        console.error('Chat receive error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.post('/chat/ack', async (req, res) => {
    try {
        const { agent, message_ids } = req.body;

        if (!agent || !message_ids || !Array.isArray(message_ids) || message_ids.length === 0) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: agent, message_ids (non-empty array of message IDs)' }
            });
        }

        const result = await pool.query(
            'UPDATE chat_messages SET acked_at = NOW() WHERE id = ANY($1) AND to_agent = $2 AND acked_at IS NULL RETURNING id',
            [message_ids, agent]
        );

        logChat('ack', { agent, requested_ids: message_ids, acked_ids: result.rows.map(r => r.id) });

        res.json({
            agent,
            acked: result.rows.length,
            acked_ids: result.rows.map(r => r.id)
        });
    } catch (err) {
        console.error('Chat ack error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

router.get('/chat/status', async (req, res) => {
    try {
        const { agent } = req.query;

        if (!agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required query param: agent' }
            });
        }

        const pending = await pool.query(
            'SELECT COUNT(*) as count FROM chat_messages WHERE to_agent = $1 AND acked_at IS NULL',
            [agent]
        );

        const latest = await pool.query(
            'SELECT MAX(id) as max_id FROM chat_messages WHERE to_agent = $1',
            [agent]
        );

        const lastActivity = await pool.query(
            'SELECT MAX(sent_at) as last_sent FROM chat_messages WHERE to_agent = $1',
            [agent]
        );

        const lastAcked = await pool.query(
            'SELECT MAX(acked_at) as last_acked FROM chat_messages WHERE to_agent = $1 AND acked_at IS NOT NULL',
            [agent]
        );

        logChat('status', { agent });

        res.json({
            agent,
            pending_count: parseInt(pending.rows[0].count),
            max_message_id: latest.rows[0].max_id,
            last_message_at: lastActivity.rows[0].last_sent,
            last_ack_at: lastAcked.rows[0].last_acked
        });
    } catch (err) {
        console.error('Chat status error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
