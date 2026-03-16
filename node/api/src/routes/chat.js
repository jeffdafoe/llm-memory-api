const { Router } = require('express');
const pool = require('../db');
const { log, logError } = require('../services/logger');
const { requireByName, resolveByName } = require('../services/actors');

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
            const fromActor = await requireByName(from_agent);
            const participants = await pool.query(
                `SELECT ac.name AS agent FROM discussion_participants dp
                 JOIN actors ac ON ac.id = dp.actor_id
                 WHERE dp.discussion_id = $1 AND dp.status = $2 AND dp.actor_id != $3`,
                [discussion_id, 'joined', fromActor.id]
            );
            for (const row of participants.rows) {
                discussionParticipants.add(row.agent);
                recipientSet.add(row.agent);
            }
        }

        if (to_agents && to_agents.length === 1 && to_agents[0] === '*') {
            const fromActor = await requireByName(from_agent);
            const known = await pool.query(
                `SELECT ac.name AS agent FROM agent_configuration agc
                 JOIN actors ac ON ac.id = agc.actor_id
                 WHERE agc.actor_id != $1`,
                [fromActor.id]
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

        // Validate all recipients exist and resolve actor_ids
        const fromActor = await requireByName(from_agent);
        const recipientActors = {};
        for (const agent of recipients) {
            const actor = await resolveByName(agent);
            if (!actor) {
                return res.status(404).json({
                    error: { code: 'NOT_FOUND', message: `Agent "${agent}" is not registered` }
                });
            }
            recipientActors[agent] = actor;
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
                'INSERT INTO chat_messages (from_actor_id, to_actor_id, message, channel) VALUES ($1, $2, $3, $4) RETURNING id, sent_at',
                [fromActor.id, recipientActors[recipient].id, msgText, ch.value]
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
        logError('chat', 'send', { agent: req.authenticatedAgent || req.body.from_agent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: 'An internal error occurred' }
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
        const actor = await requireByName(agent);
        let query = `SELECT cm.id, fa.name AS from_agent, ta.name AS to_agent, cm.message, cm.sent_at
                      FROM chat_messages cm
                      JOIN actors fa ON fa.id = cm.from_actor_id
                      JOIN actors ta ON ta.id = cm.to_actor_id
                      WHERE cm.to_actor_id = $1 AND cm.acked_at IS NULL AND cm.deleted_at IS NULL`;
        const params = [actor.id];

        if (ch.value === null) {
            query += ' AND cm.channel IS NULL';
        } else {
            params.push(ch.value);
            query += ` AND cm.channel = $${params.length}`;
        }

        if (from_agent) {
            const fromActor = await requireByName(from_agent);
            params.push(fromActor.id);
            query += ` AND cm.from_actor_id = $${params.length}`;
        }

        if (after_id !== undefined) {
            params.push(after_id);
            query += ` AND cm.id > $${params.length}`;
        }

        query += ' ORDER BY cm.id ASC';

        const result = await pool.query(query, params);

        logChat('receive', { agent, channel: ch.value, after_id: after_id || null, pending_count: result.rows.length, message_ids: result.rows.map(r => r.id) });

        res.json({
            messages: result.rows,
            pending_count: result.rows.length
        });
    } catch (err) {
        logError('chat', 'receive', { agent: req.authenticatedAgent || req.body.agent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: 'An internal error occurred' }
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

        const actor = await requireByName(agent);
        const result = await pool.query(
            'UPDATE chat_messages SET acked_at = NOW() WHERE id = ANY($1) AND to_actor_id = $2 AND acked_at IS NULL RETURNING id',
            [ids, actor.id]
        );

        logChat('ack', { agent, requested_ids: ids, acked_ids: result.rows.map(r => r.id) });

        res.json({
            agent,
            acked: result.rows.length,
            acked_ids: result.rows.map(r => r.id)
        });
    } catch (err) {
        logError('chat', 'ack', { agent: req.authenticatedAgent || req.body.agent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: 'An internal error occurred' }
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

        const actor = await requireByName(agent);
        let channelFilter;
        let params;
        if (ch.value === null) {
            channelFilter = 'AND channel IS NULL';
            params = [actor.id];
        } else {
            channelFilter = 'AND channel = $2';
            params = [actor.id, ch.value];
        }

        const pending = await pool.query(
            `SELECT COUNT(*) as count FROM chat_messages WHERE to_actor_id = $1 AND acked_at IS NULL AND deleted_at IS NULL ${channelFilter}`,
            params
        );

        const latest = await pool.query(
            `SELECT MAX(id) as max_id FROM chat_messages WHERE to_actor_id = $1 AND deleted_at IS NULL ${channelFilter}`,
            params
        );

        const lastActivity = await pool.query(
            `SELECT MAX(sent_at) as last_sent FROM chat_messages WHERE to_actor_id = $1 AND deleted_at IS NULL ${channelFilter}`,
            params
        );

        const lastAcked = await pool.query(
            `SELECT MAX(acked_at) as last_acked FROM chat_messages WHERE to_actor_id = $1 AND acked_at IS NOT NULL AND deleted_at IS NULL ${channelFilter}`,
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
        logError('chat', 'status', { agent: req.authenticatedAgent || req.body.agent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: 'An internal error occurred' }
        });
    }
});

module.exports = router;
