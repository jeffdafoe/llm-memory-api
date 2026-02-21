const { Router } = require('express');
const pool = require('../db');

const router = Router();

// Agents with last_seen within this window are considered "online"
const ONLINE_THRESHOLD_MINUTES = 5;

// Explicit heartbeat — MCP servers call this on a 2-minute interval.
// The opportunistic heartbeat middleware also updates last_seen on every
// API call, so this is mainly a fallback for idle agents.
router.post('/heartbeat', async (req, res) => {
    try {
        const { agent } = req.body;

        if (!agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
            });
        }

        const result = await pool.query(
            'UPDATE agents SET last_seen = NOW() WHERE agent = $1 RETURNING last_seen',
            [agent]
        );

        if (result.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: `Agent "${agent}" is not registered` }
            });
        }

        res.json({
            agent,
            last_seen: result.rows[0].last_seen
        });
    } catch (err) {
        console.error('Heartbeat error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

// Returns all registered agents with online/offline status and per-agent
// unread counts (chat + mail) relative to the querying agent.
// Unread chat only counts the default channel (NULL) — not discussion channels.
router.post('/presence', async (req, res) => {
    try {
        const { agent } = req.body;

        if (!agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
            });
        }

        // Single query: join agents with per-sender unread counts for
        // both chat and mail, so the caller sees everything at once.
        const result = await pool.query(
            `SELECT
                a.agent,
                a.last_seen,
                COALESCE(c.unread_count, 0)::int AS unread_chat,
                COALESCE(m.unread_count, 0)::int AS unread_mail
            FROM agents a
            LEFT JOIN (
                SELECT from_agent, COUNT(*) AS unread_count
                FROM chat_messages
                WHERE to_agent = $1 AND acked_at IS NULL AND channel IS NULL
                GROUP BY from_agent
            ) c ON c.from_agent = a.agent
            LEFT JOIN (
                SELECT from_agent, COUNT(*) AS unread_count
                FROM mail
                WHERE to_agent = $1 AND acked_at IS NULL
                GROUP BY from_agent
            ) m ON m.from_agent = a.agent
            ORDER BY a.agent`,
            [agent]
        );

        const agents = result.rows.map(row => {
            let status = 'unknown';
            if (row.last_seen) {
                const minutesAgo = (Date.now() - new Date(row.last_seen).getTime()) / 60000;
                if (minutesAgo < ONLINE_THRESHOLD_MINUTES) {
                    status = 'online';
                } else {
                    status = 'offline';
                }
            }

            return {
                agent: row.agent,
                status,
                last_seen: row.last_seen,
                unread_chat: row.unread_chat,
                unread_mail: row.unread_mail
            };
        });

        res.json({ agents });
    } catch (err) {
        console.error('Presence error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
