const { Router } = require('express');
const crypto = require('crypto');
const pool = require('../db');
const { log } = require('../services/logger');

const router = Router();

const SESSION_TTL_HOURS = 24;
const DUMMY_SALT = crypto.randomBytes(32).toString('hex');

function hashToken(plaintext, salt) {
    return crypto.pbkdf2Sync(plaintext, salt, 100000, 64, 'sha512').toString('hex');
}

function generateSessionToken() {
    return crypto.randomBytes(48).toString('base64url');
}

function logAdmin(action, details) {
    log('admin', action, details);
}

// Middleware: require authenticated user (not agent) for all admin routes except login
function requireUser(req, res, next) {
    if (req.path === '/admin/login') {
        return next();
    }
    if (!req.authenticatedUser) {
        return res.status(401).json({
            error: { code: 'UNAUTHORIZED', message: 'Admin authentication required' }
        });
    }
    next();
}

router.use(requireUser);

// POST /admin/login
router.post('/admin/login', async (req, res) => {
    const { username, password } = req.body;

    if (!username || !password) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: username, password' }
        });
    }

    try {
        const result = await pool.query(
            'SELECT id, username, password_hash, password_salt FROM users WHERE username = $1',
            [username]
        );

        const row = result.rows[0];
        const hash = hashToken(password, row ? row.password_salt : DUMMY_SALT);

        if (!row || hash !== row.password_hash) {
            logAdmin('login_failed', { username });
            return res.status(401).json({
                error: { code: 'INVALID_CREDENTIALS', message: 'Invalid username or password' }
            });
        }

        const sessionToken = generateSessionToken();
        const expiresAt = new Date(Date.now() + SESSION_TTL_HOURS * 60 * 60 * 1000);

        await pool.query(
            'INSERT INTO user_sessions (user_id, session_token, expires_at) VALUES ($1, $2, $3)',
            [row.id, sessionToken, expiresAt]
        );

        await pool.query(
            'UPDATE users SET last_login = NOW() WHERE id = $1',
            [row.id]
        );

        logAdmin('login', { username, user_id: row.id });

        res.json({
            session_token: sessionToken,
            expires_at: expiresAt.toISOString(),
            user: { id: row.id, username: row.username }
        });
    } catch (err) {
        console.error('Admin login error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Login failed' }
        });
    }
});

// POST /admin/logout
router.post('/admin/logout', async (req, res) => {
    try {
        const token = req.headers.authorization?.replace('Bearer ', '');
        if (token) {
            await pool.query(
                'DELETE FROM user_sessions WHERE session_token = $1',
                [token]
            );
        }
        logAdmin('logout', { user_id: req.authenticatedUser.id });
        res.json({ message: 'Logged out' });
    } catch (err) {
        console.error('Admin logout error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Logout failed' }
        });
    }
});

// POST /admin/dashboard — combined summary data
router.post('/admin/dashboard', async (req, res) => {
    try {
        const agents = await pool.query(
            `SELECT agent, status, last_seen, registered_at
             FROM agents
             ORDER BY agent`
        );

        const discussions = await pool.query(
            `SELECT d.id, d.topic, d.status, d.created_by, d.created_at,
                    COUNT(dp.agent) AS participant_count
             FROM discussions d
             LEFT JOIN discussion_participants dp ON dp.discussion_id = d.id
             GROUP BY d.id
             ORDER BY
                 CASE d.status WHEN 'active' THEN 0 ELSE 1 END,
                 d.created_at DESC
             LIMIT 10`
        );

        const chat = await pool.query(
            `SELECT id, from_agent, to_agent, channel, message, sent_at, acked_at
             FROM chat_messages
             WHERE from_agent != 'system'
               AND (channel IS NULL OR NOT channel LIKE 'discuss-%')
             ORDER BY
                 CASE WHEN acked_at IS NULL THEN 0 ELSE 1 END,
                 sent_at DESC
             LIMIT 15`
        );

        const mail = await pool.query(
            `SELECT id, from_agent, to_agent, subject, body, sent_at, acked_at
             FROM mail
             ORDER BY
                 CASE WHEN acked_at IS NULL THEN 0 ELSE 1 END,
                 sent_at DESC
             LIMIT 15`
        );

        const systemMessages = await pool.query(
            `SELECT id, to_agent, channel, message, sent_at, acked_at
             FROM chat_messages
             WHERE from_agent = 'system'
             ORDER BY sent_at DESC
             LIMIT 15`
        );

        res.json({
            agents: agents.rows,
            discussions: discussions.rows,
            chat: chat.rows,
            mail: mail.rows,
            system_messages: systemMessages.rows
        });
    } catch (err) {
        console.error('Admin dashboard error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to fetch dashboard data' }
        });
    }
});

// POST /admin/agents — list all agents
router.post('/admin/agents', async (req, res) => {
    try {
        const result = await pool.query(
            `SELECT agent, status, last_seen, passphrase_rotated_at, registered_at
             FROM agents
             ORDER BY agent`
        );
        res.json({ agents: result.rows });
    } catch (err) {
        console.error('Admin agents error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to fetch agents' }
        });
    }
});

// POST /admin/discussions — list discussions with optional status filter
router.post('/admin/discussions', async (req, res) => {
    const { status } = req.body;
    try {
        let sql = `
            SELECT d.id, d.topic, d.status, d.mode, d.created_by, d.created_at, d.concluded_at,
                   COUNT(dp.agent) AS participant_count
            FROM discussions d
            LEFT JOIN discussion_participants dp ON dp.discussion_id = d.id
        `;
        const params = [];
        if (status) {
            sql += ' WHERE d.status = $1';
            params.push(status);
        }
        sql += ' GROUP BY d.id ORDER BY d.created_at DESC';

        const result = await pool.query(sql, params);
        res.json({ discussions: result.rows });
    } catch (err) {
        console.error('Admin discussions error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to fetch discussions' }
        });
    }
});

// POST /admin/discussions/detail — get full discussion details
router.post('/admin/discussions/detail', async (req, res) => {
    const { discussion_id } = req.body;
    if (!discussion_id) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: discussion_id' }
        });
    }
    try {
        const discussion = await pool.query(
            'SELECT * FROM discussions WHERE id = $1',
            [discussion_id]
        );
        if (discussion.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Discussion not found' }
            });
        }
        const participants = await pool.query(
            'SELECT * FROM discussion_participants WHERE discussion_id = $1 ORDER BY agent',
            [discussion_id]
        );
        const votes = await pool.query(
            `SELECT v.*, json_agg(json_build_object('agent', b.agent, 'choice', b.choice, 'reason', b.reason, 'cast_at', b.cast_at)) AS ballots
             FROM discussion_votes v
             LEFT JOIN discussion_ballots b ON b.vote_id = v.id
             WHERE v.discussion_id = $1
             GROUP BY v.id
             ORDER BY v.id`,
            [discussion_id]
        );
        res.json({
            discussion: discussion.rows[0],
            participants: participants.rows,
            votes: votes.rows
        });
    } catch (err) {
        console.error('Admin discussion detail error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to fetch discussion details' }
        });
    }
});

// POST /admin/chat — list recent chat messages
router.post('/admin/chat', async (req, res) => {
    const { limit = 50, channel } = req.body;
    try {
        let sql = 'SELECT * FROM chat_messages';
        const params = [];
        if (channel) {
            sql += ' WHERE channel = $1';
            params.push(channel);
        }
        sql += ` ORDER BY sent_at DESC LIMIT $${params.length + 1}`;
        params.push(limit);

        const result = await pool.query(sql, params);
        res.json({ messages: result.rows });
    } catch (err) {
        console.error('Admin chat error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to fetch chat messages' }
        });
    }
});

// POST /admin/mail — list mail
router.post('/admin/mail', async (req, res) => {
    const { limit = 50 } = req.body;
    try {
        const result = await pool.query(
            'SELECT * FROM mail ORDER BY sent_at DESC LIMIT $1',
            [limit]
        );
        res.json({ messages: result.rows });
    } catch (err) {
        console.error('Admin mail error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to fetch mail' }
        });
    }
});

module.exports = router;
