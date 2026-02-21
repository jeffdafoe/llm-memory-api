const { Router } = require('express');
const pool = require('../db');

const router = Router();

router.post('/register', async (req, res) => {
    try {
        const { agent } = req.body;

        if (!agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
            });
        }

        const result = await pool.query(
            `INSERT INTO agents (agent)
            VALUES ($1)
            ON CONFLICT (agent) DO NOTHING
            RETURNING registered_at`,
            [agent]
        );

        const isNew = result.rows.length > 0;

        if (isNew) {
            const maxId = await pool.query('SELECT COALESCE(MAX(id), 0) AS max_id FROM chat_messages');
            await pool.query(
                `INSERT INTO chat_cursors (agent, last_read_id, updated_at)
                VALUES ($1, $2, NOW())
                ON CONFLICT (agent) DO NOTHING`,
                [agent, maxId.rows[0].max_id]
            );
        }

        res.json({
            agent,
            registered: isNew
        });
    } catch (err) {
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
