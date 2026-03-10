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
        let { from_agent, to_agents, discussion_id, message, channel } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (from_agent && from_agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'from_agent does not match authenticated agent' } });
            }
            from_agent = req.authenticatedAgent;
        }

        if (!from_agent || !message) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: from_agent, message' }
            });
        }

        if (!to_agents && !discussion_id) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required: to_agents (array) or discussion_id' }
            });
        }

        const ch = validateChannel(channel);
        if (!ch.valid) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Invalid channel: must match /^[a-zA-Z0-9_-]{1,50}$/' }
            });
        }

        // Resolve recipients — union of to_agents and discussion participants
        const discussionParticipants = new Set();
        const recipientSet = new Set();

        if (discussion_id) {
            const participants = await pool.query(
                'SELECT agent FROM discussion_participants WHERE discussion_id = $1 AND status = $2 AND agent != $3',
                [discussion_id, 'joined', from_agent]
            );
            for (const row of participants.rows) {
                discussionParticipants.add(row.agent);
                recipientSet.add(row.agent);
            }
        }

        if (to_agents && to_agents.length === 1 && to_agents[0] === '*') {
            const known = await pool.query(
                'SELECT agent FROM agents WHERE agent != $1',
                [from_agent]
            );
            for (const row of known.rows) {
                recipientSet.add(row.agent);
            }
        } else if (to_agents) {
            if (!Array.isArray(to_agents) || to_agents.length === 0) {
                return res.status(400).json({
                    error: { code: 'BAD_REQUEST', message: 'to_agents must be a non-empty array' }
                });
            }
            for (const agent of to_agents) {
                recipientSet.add(agent);
            }
        }

        // Remove sender from recipients (in case discussion includes them)
        recipientSet.delete(from_agent);

        const recipients = Array.from(recipientSet);
        if (recipients.length === 0) {
            return res.status(400).json({
                error: { code: 'NO_RECIPIENTS', message: 'No recipients resolved' }
            });
        }

        // Validate all recipients exist
        for (const agent of recipients) {
            const exists = await pool.query('SELECT 1 FROM agents WHERE agent = $1', [agent]);
            if (exists.rows.length === 0) {
                return res.status(404).json({
                    error: { code: 'NOT_FOUND', message: `Agent "${agent}" is not registered` }
                });
            }
        }

        // Insert a message for each recipient
        // If discussion_id is set, prefix forwarded messages for non-participants
        const results = [];
        for (const recipient of recipients) {
            let msgText = message;
            if (discussion_id && !discussionParticipants.has(recipient)) {
                msgText = `[Forwarded from discussion #${discussion_id}] ${message}`;
            }
            const result = await pool.query(
                'INSERT INTO chat_messages (from_agent, to_agent, message, channel) VALUES ($1, $2, $3, $4) RETURNING id, sent_at',
                [from_agent, recipient, msgText, ch.value]
            );
            results.push({ id: result.rows[0].id, agent: recipient, sent_at: result.rows[0].sent_at });
        }

        logChat('send', { from_agent, to_agents: recipients, message_ids: results.map(r => r.id), channel: ch.value, discussion_id: discussion_id || null });

        res.json({
            from_agent,
            to_agents: results,
            sent_at: results[0] ? results[0].sent_at : null
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
        let { agent, channel, after_id, from_agent } = req.body;

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

        if (from_agent !== undefined && (typeof from_agent !== 'string' || from_agent.length === 0)) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'from_agent must be a non-empty string' }
            });
        }

        // Build query dynamically — optional channel, after_id, and from_agent filters
        let query = 'SELECT id, from_agent, to_agent, message, sent_at FROM chat_messages WHERE to_agent = $1 AND acked_at IS NULL';
        const params = [agent];

        if (ch.value === null) {
            query += ' AND channel IS NULL';
        } else {
            params.push(ch.value);
            query += ` AND channel = $${params.length}`;
        }

        if (from_agent) {
            params.push(from_agent);
            query += ` AND from_agent = $${params.length}`;
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
                error: { code: 'BAD_REQUEST', message: 'Required fields: ids (non-empty array of message IDs)' }
            });
        }

        const result = await pool.query(
            'UPDATE chat_messages SET acked_at = NOW() WHERE id = ANY($1) AND to_agent = $2 AND acked_at IS NULL RETURNING id',
            [ids, agent]
        );

        logChat('ack', { agent, requested_ids: ids, acked_ids: result.rows.map(r => r.id) });

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
        let { agent, channel } = req.body;

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
