const { Router } = require('express');
const pool = require('../db');
const { log } = require('../services/logger');

const router = Router();

const CHANNEL_PATTERN = /^[a-zA-Z0-9_-]{1,50}$/;

function validateChannel(channel) {
    if (channel === undefined || channel === null) {
        return { valid: true, value: null };
    }
    if (typeof channel !== 'string' || !CHANNEL_PATTERN.test(channel)) {
        return { valid: false };
    }
    return { valid: true, value: channel };
}

function logChat(action, details) {
    log('chat', action, details);
}

router.post('/chat/send', async (req, res) => {
    try {
        const { from_agent, to_agent, message, channel } = req.body;

        if (!from_agent || !to_agent || !message) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: from_agent, to_agent, message' }
            });
        }

        const ch = validateChannel(channel);
        if (!ch.valid) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Invalid channel: must match /^[a-zA-Z0-9_-]{1,50}$/' }
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
                    'INSERT INTO chat_messages (from_agent, to_agent, message, channel) VALUES ($1, $2, $3, $4) RETURNING id, sent_at',
                    [from_agent, recipient, message, ch.value]
                );
                ids.push({ id: result.rows[0].id, to_agent: recipient, sent_at: result.rows[0].sent_at });
            }

            logChat('send_broadcast', { from_agent, to_agents: ids.map(i => i.to_agent), message_ids: ids.map(i => i.id), channel: ch.value });

            return res.json({
                broadcast: true,
                from_agent,
                to_agents: ids,
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
            'INSERT INTO chat_messages (from_agent, to_agent, message, channel) VALUES ($1, $2, $3, $4) RETURNING id, sent_at',
            [from_agent, to_agent, message, ch.value]
        );

        logChat('send', { from_agent, to_agent, message_id: result.rows[0].id, channel: ch.value });

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
        const { agent, channel, after_id } = req.body;

        if (!agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
            });
        }

        const ch = validateChannel(channel);
        if (!ch.valid) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Invalid channel: must match /^[a-zA-Z0-9_-]{1,50}$/' }
            });
        }

        if (after_id !== undefined && (!Number.isInteger(after_id) || after_id < 0)) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'after_id must be a non-negative integer' }
            });
        }

        // Build query dynamically — optional channel and after_id filters
        let query = 'SELECT id, from_agent, to_agent, message, sent_at FROM chat_messages WHERE to_agent = $1 AND acked_at IS NULL';
        const params = [agent];

        if (ch.value === null) {
            query += ' AND channel IS NULL';
        } else {
            params.push(ch.value);
            query += ` AND channel = $${params.length}`;
        }

        if (after_id !== undefined) {
            params.push(after_id);
            query += ` AND id > $${params.length}`;
        }

        query += ' ORDER BY id ASC';

        const result = await pool.query(query, params);

        logChat('receive', { agent, channel: ch.value, after_id: after_id || null, pending_count: result.rows.length, message_ids: result.rows.map(r => r.id) });

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

router.post('/chat/status', async (req, res) => {
    try {
        const { agent, channel } = req.body;

        if (!agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
            });
        }

        const ch = validateChannel(channel);
        if (!ch.valid) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Invalid channel: must match /^[a-zA-Z0-9_-]{1,50}$/' }
            });
        }

        let channelFilter;
        let params;
        if (ch.value === null) {
            channelFilter = 'AND channel IS NULL';
            params = [agent];
        } else {
            channelFilter = 'AND channel = $2';
            params = [agent, ch.value];
        }

        const pending = await pool.query(
            `SELECT COUNT(*) as count FROM chat_messages WHERE to_agent = $1 AND acked_at IS NULL ${channelFilter}`,
            params
        );

        const latest = await pool.query(
            `SELECT MAX(id) as max_id FROM chat_messages WHERE to_agent = $1 ${channelFilter}`,
            params
        );

        const lastActivity = await pool.query(
            `SELECT MAX(sent_at) as last_sent FROM chat_messages WHERE to_agent = $1 ${channelFilter}`,
            params
        );

        const lastAcked = await pool.query(
            `SELECT MAX(acked_at) as last_acked FROM chat_messages WHERE to_agent = $1 AND acked_at IS NOT NULL ${channelFilter}`,
            params
        );

        logChat('status', { agent, channel: ch.value });

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
