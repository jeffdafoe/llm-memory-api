const { Router } = require('express');
const crypto = require('crypto');
const pool = require('../db');
const config = require('../services/config');
const { log } = require('../services/logger');
const { hash: hashToken, generateSalt } = require('../services/hashing');
const { listNotes, readNote, saveNote, deleteNote, moveNote } = require('../services/documents');
const { searchMemory, ingestContent } = require('../services/memory');
const { getEntries: getRequestLogEntries } = require('../middleware/request-log');
const { getErrorLogEntries } = require('../services/logger');
const generatePassphrase = require('eff-diceware-passphrase');
const auth = require('../middleware/auth');
const { mailSend } = require('../services/mail');
const { formatPricing } = require('../services/provider');
const { requireByName, resolveByName, resolveById } = require('../services/actors');
const { requireAccess, getReadableNamespaces, validateNamespace } = require('../services/namespace-permissions');

const router = Router();

const SESSION_TTL_HOURS = 24;
const DUMMY_SALT = generateSalt();

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
            `SELECT agent, status, last_seen, registered_at, provider, model, virtual, active_since
             FROM agent_status
             ORDER BY CASE status WHEN 'online' THEN 0 WHEN 'available' THEN 1 WHEN 'offline' THEN 2 ELSE 3 END, last_seen DESC NULLS LAST`
        );

        const discussions = await pool.query(
            `SELECT d.id, d.topic, d.status, d.outcome, ac.name AS created_by, d.created_at,
                    COUNT(dp.actor_id) AS participant_count
             FROM discussions d
             LEFT JOIN actors ac ON ac.id = d.created_by_actor_id
             LEFT JOIN discussion_participants dp ON dp.discussion_id = d.id
             GROUP BY d.id, ac.name
             ORDER BY
                 CASE d.status WHEN 'active' THEN 0 ELSE 1 END,
                 d.created_at DESC
             LIMIT 10`
        );

        const chat = await pool.query(
            `SELECT cm.id, fa.name AS from_agent, ta.name AS to_agent, cm.channel, cm.message, cm.sent_at, cm.acked_at
             FROM chat_messages cm
             JOIN actors fa ON fa.id = cm.from_actor_id
             JOIN actors ta ON ta.id = cm.to_actor_id
             WHERE fa.name != 'system'
               AND (cm.channel IS NULL OR NOT cm.channel LIKE 'discussion-%')
             ORDER BY
                 CASE WHEN cm.acked_at IS NULL THEN 0 ELSE 1 END,
                 cm.sent_at DESC
             LIMIT 15`
        );

        const mail = await pool.query(
            `SELECT m.id, fa.name AS from_agent, ta.name AS to_agent, m.subject, m.body, m.sent_at, m.acked_at
             FROM mail m
             JOIN actors fa ON fa.id = m.from_actor_id
             JOIN actors ta ON ta.id = m.to_actor_id
             ORDER BY
                 CASE WHEN m.acked_at IS NULL THEN 0 ELSE 1 END,
                 m.sent_at DESC
             LIMIT 15`
        );

        const systemMessages = await pool.query(
            `SELECT cm.id, ta.name AS to_agent, cm.channel, cm.message, cm.sent_at, cm.acked_at
             FROM chat_messages cm
             JOIN actors fa ON fa.id = cm.from_actor_id
             JOIN actors ta ON ta.id = cm.to_actor_id
             WHERE fa.name = 'system'
             ORDER BY cm.sent_at DESC
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

// POST /admin/api-log — recent API requests from request_log table
router.post('/admin/api-log', async (req, res) => {
    const { since_id, limit } = req.body;
    try {
        const entries = await getRequestLogEntries(since_id || 0, limit || 100);
        res.json({ entries });
    } catch (err) {
        console.error('Admin api-log error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to fetch API log' }
        });
    }
});

// POST /admin/error-log — recent errors from error_log table
router.post('/admin/error-log', async (req, res) => {
    const { since_id, limit } = req.body;
    try {
        const entries = await getErrorLogEntries(since_id || 0, limit || 100);
        res.json({ entries });
    } catch (err) {
        console.error('Admin error-log error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to fetch error log' }
        });
    }
});

// POST /admin/agents — list all agents
router.post('/admin/agents', async (req, res) => {
    try {
        const result = await pool.query(
            `SELECT agent, status, last_seen, passphrase_rotated_at, registered_at, provider, model, virtual, personality, active_since,
                    cost_budget_daily, cost_budget_monthly, cache_prompts, learning_enabled, max_tokens, temperature
             FROM agent_status
             ORDER BY CASE status WHEN 'online' THEN 0 WHEN 'available' THEN 1 WHEN 'offline' THEN 2 ELSE 3 END, last_seen DESC NULLS LAST`
        );
        res.json({ agents: result.rows });
    } catch (err) {
        console.error('Admin agents error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to fetch agents' }
        });
    }
});

// POST /admin/agents/instructions/read — read an agent's startup instructions
router.post('/admin/agents/instructions/read', async (req, res) => {
    const { agent } = req.body;
    if (!agent) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
        });
    }
    try {
        const actor = await resolveByName(agent);
        if (!actor) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Agent not found' }
            });
        }
        const result = await pool.query(
            'SELECT startup_instructions FROM agents WHERE actor_id = $1',
            [actor.id]
        );
        if (result.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Agent not found' }
            });
        }
        res.json({ agent, instructions: result.rows[0].startup_instructions || '' });
    } catch (err) {
        console.error('Admin agent instructions read error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to read instructions' }
        });
    }
});

// POST /admin/agents/instructions/save — save an agent's startup instructions
router.post('/admin/agents/instructions/save', async (req, res) => {
    const { agent, content } = req.body;
    if (!agent || content === undefined) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: agent, content' }
        });
    }
    try {
        const actor = await resolveByName(agent);
        if (!actor) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Agent not found' }
            });
        }
        const result = await pool.query(
            'UPDATE agents SET startup_instructions = $1 WHERE actor_id = $2 RETURNING actor_id',
            [content, actor.id]
        );
        if (result.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Agent not found' }
            });
        }
        logAdmin('agent_instructions_save', { agent, user_id: req.authenticatedUser.id });
        res.json({ agent, message: 'Instructions saved', length: content.length });
    } catch (err) {
        console.error('Admin agent instructions save error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to save instructions' }
        });
    }
});

// POST /admin/agents/expertise/save — update an agent's expertise list
router.post('/admin/agents/expertise/save', async (req, res) => {
    const { agent, expertise } = req.body;
    if (!agent || !Array.isArray(expertise)) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: agent, expertise (array of strings)' }
        });
    }
    try {
        const cleaned = expertise
            .filter(e => typeof e === 'string' && e.trim().length > 0)
            .map(e => e.trim().toLowerCase());
        const json = JSON.stringify(cleaned);

        const actor = await resolveByName(agent);
        if (!actor) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Agent not found' }
            });
        }
        const result = await pool.query(
            'UPDATE agents SET expertise = $1 WHERE actor_id = $2 RETURNING actor_id',
            [json, actor.id]
        );
        if (result.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Agent not found' }
            });
        }
        logAdmin('agent_expertise_save', { agent, expertise: cleaned, user_id: req.authenticatedUser.id });
        res.json({ agent, expertise: cleaned, message: 'Expertise saved' });
    } catch (err) {
        console.error('Admin agent expertise save error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to save expertise' }
        });
    }
});

// POST /admin/agents/reset-passphrase — generate new passphrase, invalidate all sessions
router.post('/admin/agents/reset-passphrase', async (req, res) => {
    const { agent } = req.body;
    if (!agent) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
        });
    }
    try {
        // Generate new passphrase
        const words = generatePassphrase(3);
        const passphrase = words.join('-');
        const salt = generateSalt();
        const hash = hashToken(passphrase, salt);

        const actor = await resolveByName(agent);
        if (!actor) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Agent not found' }
            });
        }

        const result = await pool.query(
            'UPDATE agents SET token_hash = $1, token_salt = $2, passphrase_rotated_at = NOW() WHERE actor_id = $3 RETURNING actor_id',
            [hash, salt, actor.id]
        );
        if (result.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Agent not found' }
            });
        }

        // Invalidate all sessions
        await pool.query('DELETE FROM agent_sessions WHERE actor_id = $1', [actor.id]);

        // Clear cached sessions
        for (const [key, value] of auth.sessionCache.entries()) {
            if (value.agent === agent) {
                auth.sessionCache.delete(key);
            }
        }

        logAdmin('agent_passphrase_reset', { agent, user_id: req.authenticatedUser.id });

        res.json({
            agent,
            passphrase,
            message: 'Passphrase reset. All sessions invalidated.'
        });
    } catch (err) {
        console.error('Admin agent passphrase reset error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to reset passphrase' }
        });
    }
});

// POST /admin/discussions — list discussions with optional status filter
router.post('/admin/discussions', async (req, res) => {
    const { status } = req.body;
    try {
        let sql = `
            SELECT d.id, d.topic, d.status, d.mode, d.outcome, ac.name AS created_by, d.created_at, d.concluded_at,
                   COUNT(dp.actor_id) AS participant_count
            FROM discussions d
            LEFT JOIN actors ac ON ac.id = d.created_by_actor_id
            LEFT JOIN discussion_participants dp ON dp.discussion_id = d.id
        `;
        const params = [];
        if (status) {
            sql += ' WHERE d.status = $1';
            params.push(status);
        }
        sql += ' GROUP BY d.id, ac.name ORDER BY d.created_at DESC';

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
            `SELECT d.*, ac.name AS created_by
             FROM discussions d
             LEFT JOIN actors ac ON ac.id = d.created_by_actor_id
             WHERE d.id = $1`,
            [discussion_id]
        );
        if (discussion.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Discussion not found' }
            });
        }
        const participants = await pool.query(
            `SELECT dp.*, ac.name AS agent
             FROM discussion_participants dp
             JOIN actors ac ON ac.id = dp.actor_id
             WHERE dp.discussion_id = $1
             ORDER BY ac.name`,
            [discussion_id]
        );
        const votes = await pool.query(
            `SELECT v.*, pac.name AS proposed_by,
                    json_agg(json_build_object('agent', bac.name, 'choice', b.choice, 'reason', b.reason, 'cast_at', b.cast_at)) AS ballots
             FROM discussion_votes v
             LEFT JOIN actors pac ON pac.id = v.proposed_by_actor_id
             LEFT JOIN discussion_ballots b ON b.vote_id = v.id
             LEFT JOIN actors bac ON bac.id = b.actor_id
             WHERE v.discussion_id = $1
             GROUP BY v.id, pac.name
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
        let sql = `SELECT cm.id, fa.name AS from_agent, ta.name AS to_agent, cm.channel, cm.message, cm.sent_at, cm.acked_at
                   FROM chat_messages cm
                   JOIN actors fa ON fa.id = cm.from_actor_id
                   JOIN actors ta ON ta.id = cm.to_actor_id`;
        const params = [];
        if (channel) {
            sql += ' WHERE cm.channel = $1';
            params.push(channel);
        }
        sql += ` ORDER BY cm.sent_at DESC LIMIT $${params.length + 1}`;
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
            `SELECT m.id, fa.name AS from_agent, ta.name AS to_agent, m.subject, m.body, m.sent_at, m.acked_at, m.deleted_at
             FROM mail m
             JOIN actors fa ON fa.id = m.from_actor_id
             JOIN actors ta ON ta.id = m.to_actor_id
             ORDER BY m.sent_at DESC LIMIT $1`,
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

// POST /admin/mail/send — send mail to an agent from the admin dashboard
router.post('/admin/mail/send', async (req, res) => {
    const { to, subject, body } = req.body;

    if (!to || !subject || !body) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: to, subject, body' }
        });
    }

    try {
        // Verify recipient agent exists
        const toActor = await resolveByName(to);
        if (!toActor) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: `Agent "${to}" not found` }
            });
        }

        const from = req.authenticatedUser.username;
        const fromActor = await requireByName(from);
        const result = await pool.query(
            'INSERT INTO mail (to_actor_id, from_actor_id, subject, body) VALUES ($1, $2, $3, $4) RETURNING id, sent_at',
            [toActor.id, fromActor.id, subject, body]
        );

        logAdmin('mail_send', { from, to, mail_id: result.rows[0].id, subject, user_id: req.authenticatedUser.id });

        res.json({
            id: result.rows[0].id,
            sent_at: result.rows[0].sent_at,
            message: 'Mail sent'
        });
    } catch (err) {
        console.error('Admin mail send error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to send mail' }
        });
    }
});

// POST /admin/providers/registry — get provider/model registry for admin UI
router.post('/admin/providers/registry', async (req, res) => {
    try {
        const { getRegistry } = require('../services/provider');
        res.json(getRegistry());
    } catch (err) {
        console.error('Admin providers registry error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to fetch provider registry' }
        });
    }
});

// POST /admin/providers/defaults — get default configuration for a provider+model
router.post('/admin/providers/defaults', async (req, res) => {
    try {
        const { provider, model } = req.body;
        if (!provider || !model) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'provider and model are required' }
            });
        }
        const { getDefaultConfiguration } = require('../services/provider');
        const defaults = getDefaultConfiguration(provider, model);
        res.json({ defaults });
    } catch (err) {
        console.error('Admin providers defaults error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to fetch model defaults' }
        });
    }
});

// POST /admin/config/list — list all config key/value pairs
router.post('/admin/config/list', async (req, res) => {
    try {
        const result = await pool.query('SELECT key, value, description FROM config ORDER BY key');
        res.json({ config: result.rows });
    } catch (err) {
        console.error('Admin config list error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to fetch config' }
        });
    }
});

// POST /admin/config/update — update a config value by key
router.post('/admin/config/update', async (req, res) => {
    const { key, value } = req.body;
    if (!key) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: key' }
        });
    }
    try {
        const result = await pool.query(
            'UPDATE config SET value = $1 WHERE key = $2',
            [value || '', key]
        );
        if (result.rowCount === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Config key not found' }
            });
        }
        logAdmin('config_update', { key });
        res.json({ success: true });
    } catch (err) {
        console.error('Admin config update error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to update config' }
        });
    }
});

// POST /admin/notes/list — list notes in a namespace
router.post('/admin/notes/list', async (req, res) => {
    const { namespace, limit, offset, prefix } = req.body;
    if (!namespace) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: namespace' }
        });
    }
    try {
        if (req.actorId) await requireAccess(req.actorId, req.authenticatedUser.username, namespace, 'read');
        const data = await listNotes(namespace, limit || 200, offset, prefix);
        res.json(data);
    } catch (err) {
        if (err.statusCode === 403) return res.status(403).json({ error: { code: 'FORBIDDEN', message: err.message } });
        console.error('Admin notes list error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to list notes' }
        });
    }
});

// POST /admin/notes/read — read a single note
router.post('/admin/notes/read', async (req, res) => {
    const { namespace, slug } = req.body;
    if (!namespace || !slug) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug' }
        });
    }
    try {
        if (req.actorId) await requireAccess(req.actorId, req.authenticatedUser.username, namespace, 'read');
        const note = await readNote(namespace, slug);
        res.json({ note });
    } catch (err) {
        if (err.statusCode === 403) return res.status(403).json({ error: { code: 'FORBIDDEN', message: err.message } });
        if (err.statusCode === 404) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: err.message }
            });
        }
        console.error('Admin notes read error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to read note' }
        });
    }
});

// POST /admin/notes/save — save (update) a note
router.post('/admin/notes/save', async (req, res) => {
    const { namespace, slug, title, content } = req.body;
    if (!namespace || !slug || !title || content === undefined) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug, title, content' }
        });
    }
    try {
        validateNamespace(namespace);
        await requireAccess(req.actorId, req.authenticatedUser.username, namespace, 'write');
        const doc = await saveNote(namespace, title, content, slug, null);
        logAdmin('note_save', { namespace, slug, user_id: req.authenticatedUser.id });
        res.json({ note: doc });
    } catch (err) {
        if (err.statusCode === 403) {
            return res.status(403).json({
                error: { code: 'FORBIDDEN', message: err.message }
            });
        }
        console.error('Admin notes save error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to save note' }
        });
    }
});

// POST /admin/notes/delete — delete a note
router.post('/admin/notes/delete', async (req, res) => {
    const { namespace, slug } = req.body;
    if (!namespace || !slug) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug' }
        });
    }
    try {
        validateNamespace(namespace);
        await requireAccess(req.actorId, req.authenticatedUser.username, namespace, 'delete');
        await deleteNote(namespace, slug);
        logAdmin('note_delete', { namespace, slug, user_id: req.authenticatedUser.id });
        res.json({ deleted: true, namespace, slug });
    } catch (err) {
        if (err.statusCode === 403) {
            return res.status(403).json({
                error: { code: 'FORBIDDEN', message: err.message }
            });
        }
        if (err.statusCode === 404) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: err.message }
            });
        }
        console.error('Admin notes delete error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to delete note' }
        });
    }
});

// POST /admin/notes/move — rename a note's slug (and optionally namespace)
router.post('/admin/notes/move', async (req, res) => {
    const { namespace, slug, new_slug, new_namespace } = req.body;
    if (!namespace || !slug || !new_slug) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug, new_slug' }
        });
    }
    try {
        validateNamespace(namespace);
        if (new_namespace) validateNamespace(new_namespace);
        await requireAccess(req.actorId, req.authenticatedUser.username, namespace, 'write');
        if (new_namespace && new_namespace !== namespace) {
            await requireAccess(req.actorId, req.authenticatedUser.username, new_namespace, 'write');
        }
        const doc = await moveNote(namespace, slug, new_slug, new_namespace);
        logAdmin('note_move', { namespace, slug, new_slug, new_namespace: new_namespace || namespace, user_id: req.authenticatedUser.id });
        res.json({ note: doc });
    } catch (err) {
        if (err.statusCode === 403) {
            return res.status(403).json({
                error: { code: 'FORBIDDEN', message: err.message }
            });
        }
        if (err.statusCode === 404) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: err.message }
            });
        }
        if (err.statusCode === 409) {
            return res.status(409).json({
                error: { code: 'ALREADY_EXISTS', message: err.message }
            });
        }
        console.error('Admin notes move error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to move note' }
        });
    }
});

// POST /admin/notes/search — semantic search across notes
router.post('/admin/notes/search', async (req, res) => {
    const { query, namespace, limit } = req.body;
    if (!query) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: query' }
        });
    }
    try {
        const targetNs = namespace || '*';
        if (targetNs !== '*') {
            await requireAccess(req.actorId, req.authenticatedUser.username, targetNs, 'read');
        }
        // For wildcard searches, push namespace filtering into the query
        let readable = null;
        if (targetNs === '*') {
            readable = await getReadableNamespaces(req.actorId);
        }
        let data = await searchMemory(query, targetNs, limit || 10, readable);
        res.json(data);
    } catch (err) {
        if (err.statusCode === 403) {
            return res.status(403).json({
                error: { code: 'FORBIDDEN', message: err.message }
            });
        }
        console.error('Admin notes search error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Search failed' }
        });
    }
});

// POST /admin/notes/namespaces — get list of namespaces with note counts
router.post('/admin/notes/namespaces', async (req, res) => {
    try {
        const result = await pool.query(
            'SELECT namespace, COUNT(*) AS count FROM documents WHERE deleted_at IS NULL GROUP BY namespace ORDER BY namespace'
        );
        let namespaces = result.rows;
        // Filter to readable namespaces
        const readable = await getReadableNamespaces(req.actorId);
        if (readable !== null) {
            namespaces = namespaces.filter(r => readable.includes(r.namespace));
        }
        res.json({ namespaces });
    } catch (err) {
        console.error('Admin notes namespaces error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to fetch namespaces' }
        });
    }
});

// In-memory reindex state — survives tab switches and page refreshes, cleared on server restart.
let reindexState = null; // { running, current, total, chunks_created, errors, result }

// POST /admin/notes/reindex — kick off background reindex, return immediately.
router.post('/admin/notes/reindex', async (req, res) => {
    if (reindexState && reindexState.running) {
        return res.status(409).json({
            error: { code: 'CONFLICT', message: 'Reindex already in progress' }
        });
    }

    reindexState = { running: true, current: 0, total: 0, chunks_created: 0, errors: [], result: null };
    res.json({ started: true });

    // Run in background — not awaited
    (async () => {
        try {
            const deleteResult = await pool.query('DELETE FROM memory_chunks');
            const chunksDeleted = deleteResult.rowCount;

            const docs = await pool.query(
                'SELECT namespace, slug, content FROM documents WHERE deleted_at IS NULL ORDER BY namespace, slug'
            );
            reindexState.total = docs.rows.length;

            for (const doc of docs.rows) {
                try {
                    const result = await ingestContent(doc.namespace, doc.slug, doc.content);
                    reindexState.chunks_created += result.chunks_created;
                } catch (err) {
                    reindexState.errors.push({ namespace: doc.namespace, slug: doc.slug, error: err.message });
                }
                reindexState.current++;
            }

            reindexState.running = false;
            reindexState.result = {
                chunks_deleted: chunksDeleted,
                docs_indexed: reindexState.current - reindexState.errors.length,
                chunks_created: reindexState.chunks_created,
                errors: reindexState.errors
            };

            logAdmin('notes_reindex', {
                user_id: req.authenticatedUser.id,
                ...reindexState.result,
                errors: reindexState.errors.length
            });
        } catch (err) {
            console.error('Admin notes reindex error:', err.message);
            reindexState.running = false;
            reindexState.result = { error: err.message };
        }
    })();
});

// POST /admin/notes/reindex-status — poll for reindex progress.
router.post('/admin/notes/reindex-status', (req, res) => {
    if (!reindexState) {
        return res.json({ running: false });
    }
    res.json({
        running: reindexState.running,
        current: reindexState.current,
        total: reindexState.total,
        chunks_created: reindexState.chunks_created,
        errors_count: reindexState.errors.length,
        result: reindexState.result
    });
});

// POST /admin/notes/reindex-clear — dismiss completed reindex result.
router.post('/admin/notes/reindex-clear', (req, res) => {
    if (reindexState && !reindexState.running) {
        reindexState = null;
    }
    res.json({ ok: true });
});

// ---- Templates CRUD ----

const TEMPLATE_KINDS = new Set(['welcome']);

// Parse YAML-style frontmatter from template content.
// Returns { frontmatter: { key: value, ... }, body: "remaining content" }
function parseTemplateFrontmatter(content = '') {
    const match = content.match(/^---\n([\s\S]*?)\n---\n\n?([\s\S]*)$/);
    if (!match) return { frontmatter: {}, body: content };
    const frontmatter = {};
    for (const line of match[1].split('\n')) {
        const colonIdx = line.indexOf(':');
        if (colonIdx > 0) {
            const key = line.slice(0, colonIdx).trim();
            const val = line.slice(colonIdx + 1).trim();
            frontmatter[key] = val;
        }
    }
    return { frontmatter, body: match[2] };
}

// POST /admin/templates/list — list all templates, optionally filtered by kind
router.post('/admin/templates/list', async (req, res) => {
    const { kind } = req.body;
    if (kind && !TEMPLATE_KINDS.has(kind)) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Invalid kind: ' + kind }
        });
    }
    try {
        let query = 'SELECT id, name, kind, description, created_at, updated_at FROM templates';
        const params = [];
        if (kind) {
            query += ' WHERE kind = $1';
            params.push(kind);
        }
        query += ' ORDER BY name';
        const result = await pool.query(query, params);
        res.json({ templates: result.rows });
    } catch (err) {
        console.error('Admin templates list error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to list templates' }
        });
    }
});

// POST /admin/templates/read — read a single template
router.post('/admin/templates/read', async (req, res) => {
    const { id } = req.body;
    if (!id) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: id' }
        });
    }
    try {
        const result = await pool.query(
            'SELECT * FROM templates WHERE id = $1',
            [id]
        );
        if (result.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Template not found' }
            });
        }
        res.json({ template: result.rows[0] });
    } catch (err) {
        console.error('Admin templates read error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to read template' }
        });
    }
});

// POST /admin/templates/save — create or update a template
router.post('/admin/templates/save', async (req, res) => {
    const { id, name, kind, description, content } = req.body;
    if (!name || !content) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: name, content' }
        });
    }
    const templateKind = kind || 'welcome';
    if (!TEMPLATE_KINDS.has(templateKind)) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Invalid kind: ' + templateKind }
        });
    }
    // Welcome templates must have a subject in frontmatter
    if (templateKind === 'welcome') {
        const { frontmatter } = parseTemplateFrontmatter(content);
        if (!frontmatter.subject) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Welcome templates require a subject in frontmatter (e.g. ---\\nsubject: Welcome, {agent}\\n---)' }
            });
        }
    }
    try {
        let result;
        if (id) {
            // Update existing
            result = await pool.query(
                'UPDATE templates SET name = $1, kind = $2, description = $3, content = $4, updated_at = NOW() WHERE id = $5 RETURNING *',
                [name, templateKind, description || null, content, id]
            );
            if (result.rows.length === 0) {
                return res.status(404).json({
                    error: { code: 'NOT_FOUND', message: 'Template not found' }
                });
            }
        } else {
            // Check for duplicate name
            const existing = await pool.query(
                'SELECT id FROM templates WHERE name = $1',
                [name]
            );
            if (existing.rows.length > 0) {
                return res.status(409).json({
                    error: { code: 'CONFLICT', message: 'A template with that name already exists' }
                });
            }
            result = await pool.query(
                'INSERT INTO templates (name, kind, description, content) VALUES ($1, $2, $3, $4) RETURNING *',
                [name, templateKind, description || null, content]
            );
        }
        logAdmin('template_save', { template_id: result.rows[0].id, name, kind: templateKind, user_id: req.authenticatedUser.id });
        res.json({ template: result.rows[0] });
    } catch (err) {
        console.error('Admin templates save error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to save template' }
        });
    }
});

// POST /admin/templates/delete — delete a template
router.post('/admin/templates/delete', async (req, res) => {
    const { id } = req.body;
    if (!id) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: id' }
        });
    }
    try {
        const result = await pool.query(
            'DELETE FROM templates WHERE id = $1 RETURNING name',
            [id]
        );
        if (result.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Template not found' }
            });
        }
        logAdmin('template_delete', { template_id: id, name: result.rows[0].name, user_id: req.authenticatedUser.id });
        res.json({ deleted: true });
    } catch (err) {
        console.error('Admin templates delete error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to delete template' }
        });
    }
});

// Validate a cost budget value. Returns the parsed number, null (for unlimited), or throws on invalid input.
function parseCostBudget(value, fieldName) {
    if (value == null || value === '') return null;
    const parsed = Number(value);
    if (!Number.isFinite(parsed) || parsed < 0 || parsed > 99999999.99) {
        const err = new Error(`Invalid ${fieldName}: must be a non-negative number up to 99999999.99`);
        err.statusCode = 400;
        throw err;
    }
    return parsed;
}

// ---- Agent Creation ----

// POST /admin/agents/create — create an agent (active immediately) with optional welcome mail
router.post('/admin/agents/create', async (req, res) => {
    const { agent, provider, model, welcome_template_id, virtual: isVirtual, personality,
            cost_budget_daily, cost_budget_monthly,
            cache_prompts, learning_enabled, max_tokens, temperature, configuration } = req.body;

    if (!agent) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
        });
    }

    try {
        // Check if agent already exists (by actor name)
        const existingActor = await resolveByName(agent);
        if (existingActor) {
            const existing = await pool.query(
                'SELECT actor_id FROM agents WHERE actor_id = $1',
                [existingActor.id]
            );
            if (existing.rows.length > 0) {
                return res.status(409).json({
                    error: { code: 'ALREADY_EXISTS', message: 'Agent already exists' }
                });
            }
        }

        // Generate passphrase and hash it
        const words = generatePassphrase(3);
        const passphrase = words.join('-');
        const salt = generateSalt();
        const hash = hashToken(passphrase, salt);

        // Create actor if it doesn't exist
        let actorId;
        if (existingActor) {
            actorId = existingActor.id;
        } else {
            const actorResult = await pool.query(
                "INSERT INTO actors (name, type) VALUES ($1, 'agent') RETURNING id",
                [agent]
            );
            actorId = actorResult.rows[0].id;
        }

        // Create agent as active (skip pending/ack dance — admin is creating it)
        await pool.query(
            `INSERT INTO agents (actor_id, token_hash, token_salt, status, provider, model, virtual, personality, cost_budget_daily, cost_budget_monthly, cache_prompts, learning_enabled, max_tokens, temperature, configuration)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
            [actorId, hash, salt, 'active', provider || null, model || null,
             isVirtual === true, personality || null,
             parseCostBudget(cost_budget_daily, 'cost_budget_daily'),
             parseCostBudget(cost_budget_monthly, 'cost_budget_monthly'),
             cache_prompts === true, learning_enabled !== false,
             max_tokens != null ? parseInt(max_tokens) : null,
             temperature != null ? parseFloat(temperature) : null,
             configuration ? JSON.stringify(configuration) : null]
        );

        // Apply welcome template if selected
        let welcomeMailSent = false;
        if (welcome_template_id) {
            const tplResult = await pool.query(
                'SELECT content FROM templates WHERE id = $1 AND kind = $2',
                [welcome_template_id, 'welcome']
            );
            if (tplResult.rows.length > 0) {
                const rawContent = tplResult.rows[0].content;
                const { frontmatter, body: tplBody } = parseTemplateFrontmatter(rawContent);
                const mailBody = tplBody.replace(/\{agent\}/g, agent);

                // Copy template body to startup_instructions (persistent)
                await pool.query(
                    'UPDATE agents SET startup_instructions = $1 WHERE actor_id = $2',
                    [mailBody, actorId]
                );

                // Also send as welcome mail (fallback / immediate context)
                const mailSubject = (frontmatter.subject || 'Welcome, {agent}').replace(/\{agent\}/g, agent);
                await mailSend(agent, 'system', mailSubject, mailBody);
                welcomeMailSent = true;
            }
        }

        logAdmin('agent_create', { agent, virtual: isVirtual === true, welcome_mail: welcomeMailSent, user_id: req.authenticatedUser.id });

        res.json({
            agent,
            passphrase,
            virtual: isVirtual === true,
            status: 'active',
            welcome_mail_sent: welcomeMailSent,
            message: isVirtual ? 'Virtual agent created.' : 'Agent created. Save the passphrase — it will not be shown again.'
        });
    } catch (err) {
        if (err.statusCode === 400) {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: err.message } });
        }
        console.error('Admin agent create error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to create agent' }
        });
    }
});

// POST /admin/agents/read — get full agent detail (includes configuration JSON)
router.post('/admin/agents/read', async (req, res) => {
    const { agent } = req.body;
    if (!agent) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required field: agent' } });
    }
    try {
        const actor = await resolveByName(agent);
        if (!actor) {
            return res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Agent not found' } });
        }
        const result = await pool.query(
            `SELECT ac.name AS agent, a.provider, a.model, a.virtual, a.personality, a.configuration, a.expertise,
                    a.cache_prompts, a.learning_enabled, a.max_tokens, a.temperature,
                    a.cost_budget_daily, a.cost_budget_monthly, a.api_key IS NOT NULL AS has_api_key
             FROM agents a
             JOIN actors ac ON ac.id = a.actor_id
             WHERE a.actor_id = $1`,
            [actor.id]
        );
        if (result.rows.length === 0) {
            return res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Agent not found' } });
        }
        const row = result.rows[0];

        // Compute current cost totals from usage log
        const costResult = await pool.query(
            `SELECT
                COALESCE(SUM(CASE WHEN created_at >= DATE_TRUNC('day', NOW() AT TIME ZONE 'UTC') THEN cost ELSE 0 END), 0) AS cost_today,
                COALESCE(SUM(cost), 0) AS cost_monthly
             FROM virtual_agent_usage
             WHERE actor_id = $1 AND created_at >= NOW() - INTERVAL '30 days'`,
            [actor.id]
        );
        row.cost_today = parseFloat(costResult.rows[0].cost_today);
        row.cost_monthly = parseFloat(costResult.rows[0].cost_monthly);

        // Add pricing info string for display
        if (row.provider && row.model) {
            row.pricing_info = formatPricing(row.provider, row.model);
        }

        res.json(row);
    } catch (err) {
        console.error('Admin agent read error:', err.message);
        res.status(500).json({ error: { code: 'INTERNAL', message: 'Failed to read agent' } });
    }
});

// POST /admin/agents/update — update virtual agent config
router.post('/admin/agents/update', async (req, res) => {
    const { agent, personality, api_key, configuration, provider, model,
            cost_budget_daily, cost_budget_monthly,
            cache_prompts, learning_enabled, max_tokens, temperature } = req.body;

    if (!agent) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
        });
    }

    try {
        const actor = await resolveByName(agent);
        if (!actor) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Agent not found' }
            });
        }
        const existing = await pool.query('SELECT actor_id, virtual FROM agents WHERE actor_id = $1', [actor.id]);
        if (existing.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Agent not found' }
            });
        }

        const updates = [];
        const params = [];
        let idx = 1;

        if (personality !== undefined) {
            params.push(personality || null);
            updates.push(`personality = $${idx++}`);
        }
        if (api_key !== undefined) {
            // Encrypt the API key before storing
            if (api_key) {
                const { encryptApiKey } = require('../services/provider');
                params.push(encryptApiKey(api_key));
            } else {
                params.push(null);
            }
            updates.push(`api_key = $${idx++}`);
        }
        if (configuration !== undefined) {
            params.push(configuration ? JSON.stringify(configuration) : null);
            updates.push(`configuration = $${idx++}`);
        }
        if (provider !== undefined) {
            params.push(provider || null);
            updates.push(`provider = $${idx++}`);
        }
        if (model !== undefined) {
            params.push(model || null);
            updates.push(`model = $${idx++}`);
        }
        if (cost_budget_daily !== undefined) {
            params.push(parseCostBudget(cost_budget_daily, 'cost_budget_daily'));
            updates.push(`cost_budget_daily = $${idx++}`);
        }
        if (cost_budget_monthly !== undefined) {
            params.push(parseCostBudget(cost_budget_monthly, 'cost_budget_monthly'));
            updates.push(`cost_budget_monthly = $${idx++}`);
        }
        if (cache_prompts !== undefined) {
            params.push(cache_prompts === true);
            updates.push(`cache_prompts = $${idx++}`);
        }
        if (learning_enabled !== undefined) {
            params.push(learning_enabled !== false);
            updates.push(`learning_enabled = $${idx++}`);
        }
        if (max_tokens !== undefined) {
            params.push(max_tokens === null || max_tokens === '' ? null : parseInt(max_tokens));
            updates.push(`max_tokens = $${idx++}`);
        }
        if (temperature !== undefined) {
            params.push(temperature === null || temperature === '' ? null : parseFloat(temperature));
            updates.push(`temperature = $${idx++}`);
        }

        if (updates.length === 0) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'No fields to update' }
            });
        }

        params.push(actor.id);
        await pool.query(
            `UPDATE agents SET ${updates.join(', ')} WHERE actor_id = $${idx}`,
            params
        );

        logAdmin('agent_update', { agent, fields: updates.map(u => u.split(' ')[0]), user_id: req.authenticatedUser.id });

        res.json({ agent, updated: true });
    } catch (err) {
        if (err.statusCode === 400) {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: err.message } });
        }
        console.error('Admin agent update error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to update agent' }
        });
    }
});

// POST /admin/agents/usage — get usage history for an agent
router.post('/admin/agents/usage', async (req, res) => {
    const { agent, limit } = req.body;
    if (!agent) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required field: agent' } });
    }
    try {
        const actor = await resolveByName(agent);
        if (!actor) {
            return res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Agent not found' } });
        }
        const result = await pool.query(
            `SELECT id, provider, model, input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost, context, created_at
             FROM virtual_agent_usage
             WHERE actor_id = $1
             ORDER BY created_at DESC
             LIMIT $2`,
            [actor.id, Math.min(parseInt(limit) || 50, 200)]
        );
        res.json({ usage: result.rows });
    } catch (err) {
        console.error('Admin agent usage error:', err.message);
        res.status(500).json({ error: { code: 'INTERNAL', message: 'Failed to fetch usage data' } });
    }
});

module.exports = router;
