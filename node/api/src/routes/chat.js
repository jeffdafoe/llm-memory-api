const { Router } = require('express');
const pool = require('../db');

const router = Router();

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

        const cursor = await pool.query(
            'SELECT last_read_id FROM chat_cursors WHERE agent = $1',
            [agent]
        );
        const lastReadId = cursor.rows.length > 0 ? cursor.rows[0].last_read_id : 0;

        const result = await pool.query(
            'SELECT id, from_agent, to_agent, message, sent_at FROM chat_messages WHERE to_agent = $1 AND id > $2 ORDER BY id ASC',
            [agent, lastReadId]
        );

        res.json({
            messages: result.rows,
            last_read_id: lastReadId
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
        const { agent, last_read_id } = req.body;

        if (!agent || last_read_id == null) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: agent, last_read_id' }
            });
        }

        await pool.query(
            `INSERT INTO chat_cursors (agent, last_read_id, updated_at)
            VALUES ($1, $2, NOW())
            ON CONFLICT (agent) DO UPDATE
            SET last_read_id = GREATEST(chat_cursors.last_read_id, $2),
                updated_at = NOW()`,
            [agent, last_read_id]
        );

        res.json({
            agent,
            last_read_id
        });
    } catch (err) {
        console.error('Chat ack error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
