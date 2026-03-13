const { Router } = require('express');
const crypto = require('crypto');
const pool = require('../db');
const config = require('../services/config');
const { log } = require('../services/logger');
const { hash: hashToken, generateSalt, verify } = require('../services/hashing');
const { listNotes, readNote, saveNote, deleteNote, moveNote } = require('../services/documents');
const { searchMemory, ingestContent } = require('../services/memory');
const { getEntries: getRequestLogEntries } = require('../middleware/request-log');
const { getErrorLogEntries } = require('../services/logger');
const generatePassphrase = require('eff-diceware-passphrase');
const auth = require('../middleware/auth');
const { mailSend } = require('../services/mail');
const { formatPricing } = require('../services/provider');
const { requireByName, resolveByName, resolveById } = require('../services/actors');
const { requireAccess, getReadableNamespaces, validateNamespace, clearCache: clearPermissionsCache } = require('../services/namespace-permissions');
const { SESSION_KIND } = require('../constants');
const { getVisibleActorIds, canSee, clearCache: clearVisibilityCache } = require('../services/actor-visibility');

const router = Router();

const SESSION_TTL_HOURS = 24;
const DUMMY_SALT = generateSalt();

function generateSessionToken() {
    return crypto.randomBytes(48).toString('base64url');
}

function logAdmin(action, details) {
    log('admin', action, details);
}

// Check if the authenticated user can see a specific agent (by name).
// Returns the resolved actor, or sends 404 and returns null.
// Hidden actors are indistinguishable from nonexistent ones (no 403).
async function requireVisibility(req, res, agentName) {
    const actor = await resolveByName(agentName);
    if (!actor) {
        res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Agent not found' } });
        return null;
    }
    const allowed = await canSee(req.actorId, actor.id);
    if (!allowed) {
        res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Agent not found' } });
        return null;
    }
    return actor;
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
            "SELECT id, name AS username, password_hash, password_salt FROM actors WHERE name = $1 AND password_hash IS NOT NULL",
            [username]
        );

        const row = result.rows[0];

        // Compute hash even when row is missing (timing-safe rejection)
        if (!row) {
            hashToken(password, DUMMY_SALT);
            logAdmin('login_failed', { username });
            return res.status(401).json({
                error: { code: 'INVALID_CREDENTIALS', message: 'Invalid username or password' }
            });
        }

        if (!verify(password, row.password_salt, row.password_hash)) {
            logAdmin('login_failed', { username });
            return res.status(401).json({
                error: { code: 'INVALID_CREDENTIALS', message: 'Invalid username or password' }
            });
        }

        const sessionToken = generateSessionToken();
        const salt = generateSalt();
        const tokenHash = hashToken(sessionToken, salt);
        const expiresAt = new Date(Date.now() + SESSION_TTL_HOURS * 60 * 60 * 1000);

        await pool.query(
            "INSERT INTO sessions (actor_id, token_hash, token_salt, kind, expires_at) VALUES ($1, $2, $3, $4, $5)",
            [row.id, tokenHash, salt, SESSION_KIND.WEB, expiresAt]
        );

        await pool.query(
            'UPDATE actors SET last_seen = NOW() WHERE id = $1',
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

        // Find and delete only the session matching this token (not all web sessions)
        if (token) {
            const sessions = await pool.query(
                "SELECT id, token_hash, token_salt FROM sessions WHERE actor_id = $1 AND kind = $2",
                [req.authenticatedUser.id, SESSION_KIND.WEB]
            );

            for (const row of sessions.rows) {
                if (verify(token, row.token_salt, row.token_hash)) {
                    await pool.query('DELETE FROM sessions WHERE id = $1', [row.id]);
                    break;
                }
            }

            auth.sessionCache.delete(token);
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

// POST /admin/change-password — change the logged-in user's password
router.post('/admin/change-password', async (req, res) => {
    const { current_password, new_password } = req.body;

    if (!current_password || !new_password) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: current_password, new_password' }
        });
    }

    if (new_password.length < 4) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Password must be at least 4 characters' }
        });
    }

    try {
        const result = await pool.query(
            'SELECT password_hash, password_salt FROM actors WHERE id = $1',
            [req.authenticatedUser.id]
        );
        const row = result.rows[0];

        if (!row || !row.password_hash || !verify(current_password, row.password_salt, row.password_hash)) {
            return res.status(401).json({
                error: { code: 'INVALID_CREDENTIALS', message: 'Current password is incorrect' }
            });
        }

        const salt = generateSalt();
        const hash = hashToken(new_password, salt);
        await pool.query(
            'UPDATE actors SET password_hash = $1, password_salt = $2 WHERE id = $3',
            [hash, salt, req.authenticatedUser.id]
        );

        logAdmin('password_changed', { user_id: req.authenticatedUser.id });
        res.json({ message: 'Password changed' });
    } catch (err) {
        console.error('Admin change-password error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to change password' }
        });
    }
});

// POST /admin/dashboard — combined summary data
router.post('/admin/dashboard', async (req, res) => {
    try {
        const visibleIds = await getVisibleActorIds(req.actorId);
        const hasFilter = visibleIds !== null;
        const idsArray = hasFilter ? Array.from(visibleIds) : [];

        let agentsSql = `SELECT agent, status, last_seen, registered_at, provider, model, virtual, active_since
             FROM agent_status`;
        const agentsParams = [];
        if (hasFilter) {
            agentsSql += ' WHERE actor_id = ANY($1)';
            agentsParams.push(idsArray);
        }
        agentsSql += ` ORDER BY CASE status WHEN 'online' THEN 0 WHEN 'available' THEN 1 WHEN 'offline' THEN 2 ELSE 3 END, last_seen DESC NULLS LAST`;
        const agents = await pool.query(agentsSql, agentsParams);

        let discussionsSql = `
            SELECT d.id, d.topic, d.status, d.outcome, ac.name AS created_by, d.created_at,
                    COUNT(dp.actor_id) AS participant_count
             FROM discussions d
             LEFT JOIN actors ac ON ac.id = d.created_by_actor_id
             LEFT JOIN discussion_participants dp ON dp.discussion_id = d.id`;
        const discussionsParams = [];
        if (hasFilter) {
            discussionsSql += ` WHERE NOT EXISTS (
                SELECT 1 FROM discussion_participants dp2
                WHERE dp2.discussion_id = d.id AND dp2.actor_id != ALL($1))`;
            discussionsParams.push(idsArray);
        }
        discussionsSql += ` GROUP BY d.id, ac.name
             ORDER BY CASE d.status WHEN 'active' THEN 0 ELSE 1 END, d.created_at DESC
             LIMIT 10`;
        const discussions = await pool.query(discussionsSql, discussionsParams);

        let chatSql = `SELECT cm.id, fa.name AS from_agent, ta.name AS to_agent, cm.channel, cm.message, cm.sent_at, cm.acked_at
             FROM chat_messages cm
             JOIN actors fa ON fa.id = cm.from_actor_id
             JOIN actors ta ON ta.id = cm.to_actor_id
             WHERE fa.name != 'system'
               AND (cm.channel IS NULL OR NOT cm.channel LIKE 'discussion-%')`;
        const chatParams = [];
        if (hasFilter) {
            chatSql += ' AND cm.from_actor_id = ANY($1) AND cm.to_actor_id = ANY($1)';
            chatParams.push(idsArray);
        }
        chatSql += ` ORDER BY CASE WHEN cm.acked_at IS NULL THEN 0 ELSE 1 END, cm.sent_at DESC LIMIT 15`;
        const chat = await pool.query(chatSql, chatParams);

        let mailSql = `SELECT m.id, fa.name AS from_agent, ta.name AS to_agent, m.subject, m.body, m.sent_at, m.acked_at
             FROM mail m
             JOIN actors fa ON fa.id = m.from_actor_id
             JOIN actors ta ON ta.id = m.to_actor_id`;
        const mailParams = [];
        if (hasFilter) {
            mailSql += ' WHERE m.from_actor_id = ANY($1) AND m.to_actor_id = ANY($1)';
            mailParams.push(idsArray);
        }
        mailSql += ` ORDER BY CASE WHEN m.acked_at IS NULL THEN 0 ELSE 1 END, m.sent_at DESC LIMIT 15`;
        const mail = await pool.query(mailSql, mailParams);

        let sysMsgSql = `SELECT cm.id, ta.name AS to_agent, cm.channel, cm.message, cm.sent_at, cm.acked_at
             FROM chat_messages cm
             JOIN actors fa ON fa.id = cm.from_actor_id
             JOIN actors ta ON ta.id = cm.to_actor_id
             WHERE fa.name = 'system'`;
        const sysMsgParams = [];
        if (hasFilter) {
            sysMsgSql += ' AND cm.to_actor_id = ANY($1)';
            sysMsgParams.push(idsArray);
        }
        sysMsgSql += ' ORDER BY cm.sent_at DESC LIMIT 15';
        const systemMessages = await pool.query(sysMsgSql, sysMsgParams);

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
        const visibleIds = await getVisibleActorIds(req.actorId);
        const entries = await getRequestLogEntries(since_id || 0, limit || 100, visibleIds);
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
        const visibleIds = await getVisibleActorIds(req.actorId);
        const entries = await getErrorLogEntries(since_id || 0, limit || 100, visibleIds);
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
        const visibleIds = await getVisibleActorIds(req.actorId);
        let sql = `SELECT agent, actor_id, status, last_seen, passphrase_rotated_at, registered_at, provider, model, virtual, personality, active_since,
                    cost_budget_daily, cost_budget_monthly, cache_prompts, learning_enabled, max_tokens, temperature
             FROM agent_status`;
        const params = [];
        if (visibleIds !== null) {
            sql += ' WHERE actor_id = ANY($1)';
            params.push(Array.from(visibleIds));
        }
        sql += ` ORDER BY CASE status WHEN 'online' THEN 0 WHEN 'available' THEN 1 WHEN 'offline' THEN 2 ELSE 3 END, last_seen DESC NULLS LAST`;
        const result = await pool.query(sql, params);
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
        const actor = await requireVisibility(req, res, agent);
        if (!actor) return;
        const result = await pool.query(
            'SELECT startup_instructions FROM agent_configuration WHERE actor_id = $1',
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
        const actor = await requireVisibility(req, res, agent);
        if (!actor) return;
        const result = await pool.query(
            'UPDATE agent_configuration SET startup_instructions = $1 WHERE actor_id = $2 RETURNING actor_id',
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

        const actor = await requireVisibility(req, res, agent);
        if (!actor) return;
        const result = await pool.query(
            'UPDATE actors SET expertise = $1 WHERE id = $2 RETURNING id',
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

        const actor = await requireVisibility(req, res, agent);
        if (!actor) return;

        const result = await pool.query(
            'UPDATE actors SET token_hash = $1, token_salt = $2, passphrase_rotated_at = NOW() WHERE id = $3 RETURNING id',
            [hash, salt, actor.id]
        );
        if (result.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Agent not found' }
            });
        }

        // Invalidate all sessions
        await pool.query("DELETE FROM sessions WHERE actor_id = $1 AND kind = $2", [actor.id, SESSION_KIND.API]);

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
        const visibleIds = await getVisibleActorIds(req.actorId);
        const hasFilter = visibleIds !== null;

        let sql = `
            SELECT d.id, d.topic, d.status, d.mode, d.outcome, ac.name AS created_by, d.created_at, d.concluded_at,
                   COUNT(dp.actor_id) AS participant_count
            FROM discussions d
            LEFT JOIN actors ac ON ac.id = d.created_by_actor_id
            LEFT JOIN discussion_participants dp ON dp.discussion_id = d.id
        `;
        const conditions = [];
        const params = [];
        if (status) {
            params.push(status);
            conditions.push('d.status = $' + params.length);
        }
        if (hasFilter) {
            params.push(Array.from(visibleIds));
            conditions.push(`NOT EXISTS (
                SELECT 1 FROM discussion_participants dp2
                WHERE dp2.discussion_id = d.id AND dp2.actor_id != ALL($${params.length}))`);
        }
        if (conditions.length > 0) {
            sql += ' WHERE ' + conditions.join(' AND ');
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
        // Check visibility: ALL participants must be visible to the viewer
        const visibleIds = await getVisibleActorIds(req.actorId);
        if (visibleIds !== null) {
            const hiddenCheck = await pool.query(
                `SELECT 1 FROM discussion_participants
                 WHERE discussion_id = $1 AND actor_id != ALL($2) LIMIT 1`,
                [discussion_id, Array.from(visibleIds)]
            );
            if (hiddenCheck.rows.length > 0) {
                return res.status(404).json({
                    error: { code: 'NOT_FOUND', message: 'Discussion not found' }
                });
            }
        }

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
        const visibleIds = await getVisibleActorIds(req.actorId);
        const hasFilter = visibleIds !== null;

        let sql = `SELECT cm.id, fa.name AS from_agent, ta.name AS to_agent, cm.channel, cm.message, cm.sent_at, cm.acked_at
                   FROM chat_messages cm
                   JOIN actors fa ON fa.id = cm.from_actor_id
                   JOIN actors ta ON ta.id = cm.to_actor_id`;
        const conditions = [];
        const params = [];
        if (channel) {
            params.push(channel);
            conditions.push('cm.channel = $' + params.length);
        }
        if (hasFilter) {
            params.push(Array.from(visibleIds));
            conditions.push('cm.from_actor_id = ANY($' + params.length + ') AND cm.to_actor_id = ANY($' + params.length + ')');
        }
        if (conditions.length > 0) {
            sql += ' WHERE ' + conditions.join(' AND ');
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
        const visibleIds = await getVisibleActorIds(req.actorId);
        const hasFilter = visibleIds !== null;

        let sql = `SELECT m.id, fa.name AS from_agent, ta.name AS to_agent, m.subject, m.body, m.sent_at, m.acked_at, m.deleted_at
             FROM mail m
             JOIN actors fa ON fa.id = m.from_actor_id
             JOIN actors ta ON ta.id = m.to_actor_id`;
        const params = [];
        if (hasFilter) {
            params.push(Array.from(visibleIds));
            sql += ' WHERE m.from_actor_id = ANY($1) AND m.to_actor_id = ANY($1)';
        }
        sql += ` ORDER BY m.sent_at DESC LIMIT $${params.length + 1}`;
        params.push(limit);

        const result = await pool.query(sql, params);
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
        // Verify recipient agent exists and is visible to the sender
        const toActor = await requireVisibility(req, res, to);
        if (!toActor) return;

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
        validateNamespace(namespace);
        await requireAccess(req.actorId, req.authenticatedUser.username, 'user', namespace, 'read');
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
        validateNamespace(namespace);
        await requireAccess(req.actorId, req.authenticatedUser.username, 'user', namespace, 'read');
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
        await requireAccess(req.actorId, req.authenticatedUser.username, 'user', namespace, 'write');
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
        await requireAccess(req.actorId, req.authenticatedUser.username, 'user', namespace, 'delete');
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
        await requireAccess(req.actorId, req.authenticatedUser.username, 'user', namespace, 'write');
        if (new_namespace && new_namespace !== namespace) {
            await requireAccess(req.actorId, req.authenticatedUser.username, 'user', new_namespace, 'write');
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
            validateNamespace(targetNs);
            await requireAccess(req.actorId, req.authenticatedUser.username, 'user', targetNs, 'read');
        }
        // For wildcard searches, push namespace filtering into the query
        let readable = null;
        if (targetNs === '*') {
            readable = await getReadableNamespaces(req.actorId, req.authenticatedUser.username, 'user');
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
        const readable = await getReadableNamespaces(req.actorId, req.authenticatedUser.username, 'user');
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

// ---- Actor Creation ----

// POST /admin/actors/create — create an actor (agent + optional UI user) with optional welcome mail
router.post('/admin/actors/create', async (req, res) => {
    const { name, provider, model, welcome_template_id, virtual: isVirtual, personality,
            cost_budget_daily, cost_budget_monthly,
            cache_prompts, learning_enabled, max_tokens, temperature, configuration,
            ui_access, password } = req.body;

    if (!name || !name.trim()) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: name' }
        });
    }
    const actorName = name.trim().toLowerCase();

    // Validate UI access fields
    if (ui_access) {
        if (!password || password.length < 4) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Password must be at least 4 characters' }
            });
        }
    }

    // Validate max_tokens and temperature if provided
    if (max_tokens != null) {
        const parsed = parseInt(max_tokens);
        if (isNaN(parsed) || parsed < 1) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'max_tokens must be a positive integer' }
            });
        }
    }
    if (temperature != null) {
        const parsed = parseFloat(temperature);
        if (isNaN(parsed) || parsed < 0 || parsed > 2) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'temperature must be a number between 0 and 2' }
            });
        }
    }

    try {
        // Check if actor already exists
        const existingActor = await resolveByName(actorName);
        if (existingActor) {
            return res.status(409).json({
                error: { code: 'ALREADY_EXISTS', message: 'Actor already exists: ' + actorName }
            });
        }

        // Generate passphrase and hash it (all actors get a passphrase for API auth)
        const words = generatePassphrase(3);
        const passphrase = words.join('-');
        const passphraseSalt = generateSalt();
        const passphraseHash = hashToken(passphrase, passphraseSalt);

        // Optionally hash the UI password
        let passwordHash = null;
        let passwordSalt = null;
        if (ui_access && password) {
            passwordSalt = generateSalt();
            passwordHash = hashToken(password, passwordSalt);
        }

        const client = await pool.connect();
        let actorId;
        try {
            await client.query('BEGIN');

            // Create actor
            const actorResult = await client.query(
                `INSERT INTO actors (name, token_hash, token_salt, password_hash, password_salt, status)
                 VALUES ($1, $2, $3, $4, $5, 'active') RETURNING id`,
                [actorName, passphraseHash, passphraseSalt, passwordHash, passwordSalt]
            );
            actorId = actorResult.rows[0].id;

            // Create agent configuration
            await client.query(
                `INSERT INTO agent_configuration (actor_id, provider, model, virtual, personality, cost_budget_daily, cost_budget_monthly, cache_prompts, learning_enabled, max_tokens, temperature, configuration)
                 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
                [actorId, provider || null, model || null,
                 isVirtual === true, personality || null,
                 parseCostBudget(cost_budget_daily, 'cost_budget_daily'),
                 parseCostBudget(cost_budget_monthly, 'cost_budget_monthly'),
                 cache_prompts === true, learning_enabled !== false,
                 max_tokens != null ? parseInt(max_tokens) : null,
                 temperature != null ? parseFloat(temperature) : null,
                 configuration ? JSON.stringify(configuration) : null]
            );

            await client.query('COMMIT');
        } catch (txErr) {
            await client.query('ROLLBACK');
            throw txErr;
        } finally {
            client.release();
        }

        // Apply welcome template if selected (non-virtual only)
        // Wrapped in its own try/catch — actor is already committed at this point
        let welcomeMailSent = false;
        let postCommitError = null;
        if (welcome_template_id && !isVirtual) {
            try {
                const tplResult = await pool.query(
                    'SELECT content FROM templates WHERE id = $1 AND kind = $2',
                    [welcome_template_id, 'welcome']
                );
                if (tplResult.rows.length > 0) {
                    const rawContent = tplResult.rows[0].content;
                    const { frontmatter, body: tplBody } = parseTemplateFrontmatter(rawContent);
                    const mailBody = tplBody.replace(/\{agent\}/g, actorName);

                    // Copy template body to startup_instructions (persistent)
                    await pool.query(
                        'UPDATE agent_configuration SET startup_instructions = $1 WHERE actor_id = $2',
                        [mailBody, actorId]
                    );

                    // Also send as welcome mail (fallback / immediate context)
                    const mailSubject = (frontmatter.subject || 'Welcome, {agent}').replace(/\{agent\}/g, actorName);
                    await mailSend(actorName, 'system', mailSubject, mailBody);
                    welcomeMailSent = true;
                }
            } catch (postErr) {
                console.error('Post-commit welcome template error:', postErr.message);
                postCommitError = 'Actor created but welcome template failed: ' + postErr.message;
            }
        }

        logAdmin('actor_create', { name: actorName, virtual: isVirtual === true, ui_access: !!ui_access, welcome_mail: welcomeMailSent, user_id: req.authenticatedUser.id });

        const response = {
            name: actorName,
            passphrase,
            virtual: isVirtual === true,
            ui_access: !!ui_access,
            status: 'active',
            welcome_mail_sent: welcomeMailSent,
            message: isVirtual ? 'Virtual agent created.' : 'Actor created. Save the passphrase — it will not be shown again.'
        };
        if (postCommitError) {
            response.warning = postCommitError;
        }
        res.json(response);
    } catch (err) {
        if (err.statusCode === 400) {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: err.message } });
        }
        console.error('Admin actor create error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL', message: 'Failed to create actor' }
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
        const actor = await requireVisibility(req, res, agent);
        if (!actor) return;
        const result = await pool.query(
            `SELECT ac.name AS agent, agc.provider, agc.model, agc.virtual, agc.personality, agc.configuration, ac.expertise,
                    agc.cache_prompts, agc.learning_enabled, agc.max_tokens, agc.temperature,
                    agc.cost_budget_daily, agc.cost_budget_monthly, agc.api_key IS NOT NULL AS has_api_key
             FROM agent_configuration agc
             JOIN actors ac ON ac.id = agc.actor_id
             WHERE agc.actor_id = $1`,
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
        const actor = await requireVisibility(req, res, agent);
        if (!actor) return;
        const existing = await pool.query('SELECT actor_id, virtual FROM agent_configuration WHERE actor_id = $1', [actor.id]);
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
            `UPDATE agent_configuration SET ${updates.join(', ')} WHERE actor_id = $${idx}`,
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
        const actor = await requireVisibility(req, res, agent);
        if (!actor) return;
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

// ─── Actor permissions & visibility management ───

// Helper: parse and validate actor_id from request body. Returns integer or sends 400.
function parseActorId(raw, res) {
    const id = parseInt(raw, 10);
    if (!Number.isInteger(id) || id <= 0) {
        res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Invalid actor_id' } });
        return null;
    }
    return id;
}

// POST /admin/actors/list — list all actors (for the Actors config tab)
router.post('/admin/actors/list', async (req, res) => {
    try {
        const result = await pool.query(
            `SELECT a.id, a.name, a.created_at,
                    (ac.actor_id IS NOT NULL) AS is_agent,
                    (a.password_hash IS NOT NULL) AS is_user
             FROM actors a
             LEFT JOIN agent_configuration ac ON ac.actor_id = a.id
             ORDER BY a.name`
        );
        res.json({ actors: result.rows });
    } catch (err) {
        console.error('Admin actors list error:', err.message);
        res.status(500).json({ error: { code: 'INTERNAL', message: 'Failed to fetch actors' } });
    }
});

// POST /admin/actors/permissions/read — get namespace permissions for one actor
router.post('/admin/actors/permissions/read', async (req, res) => {
    const actorId = parseActorId(req.body.actor_id, res);
    if (actorId === null) return;
    try {
        // Verify actor exists
        const actor = await resolveById(actorId);
        if (!actor) {
            return res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Actor not found' } });
        }
        const result = await pool.query(
            'SELECT namespace, can_read, can_write, can_delete FROM namespace_permissions WHERE actor_id = $1 ORDER BY namespace',
            [actorId]
        );
        res.json({ permissions: result.rows });
    } catch (err) {
        console.error('Admin permissions read error:', err.message);
        res.status(500).json({ error: { code: 'INTERNAL', message: 'Failed to fetch permissions' } });
    }
});

// POST /admin/actors/permissions/save — full replace of namespace permissions for one actor
// Body: { actor_id, permissions: [{ namespace, can_read, can_write, can_delete }] }
router.post('/admin/actors/permissions/save', async (req, res) => {
    const actorId = parseActorId(req.body.actor_id, res);
    if (actorId === null) return;
    const { permissions } = req.body;
    if (!Array.isArray(permissions)) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required field: permissions (array)' } });
    }

    // Verify actor exists
    const actor = await resolveById(actorId);
    if (!actor) {
        return res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Actor not found' } });
    }

    // Normalize first, then validate
    const normalized = permissions.map(p => ({
        namespace: (typeof p.namespace === 'string') ? p.namespace.trim() : '',
        can_read: !!p.can_read,
        can_write: !!p.can_write,
        can_delete: !!p.can_delete
    }));

    // Validate each namespace using the shared helper (allows '/' wildcard, rejects '*' and empty)
    for (const perm of normalized) {
        if (perm.namespace === '/') continue; // wildcard is allowed in permissions
        try {
            validateNamespace(perm.namespace);
        } catch (err) {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: err.message } });
        }
        if (!perm.namespace) {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Empty namespace not allowed' } });
        }
    }

    // Check for duplicates after normalization
    const namespaces = normalized.map(p => p.namespace);
    if (new Set(namespaces).size !== namespaces.length) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Duplicate namespace entries' } });
    }

    const client = await pool.connect();
    try {
        await client.query('BEGIN');
        await client.query('DELETE FROM namespace_permissions WHERE actor_id = $1', [actorId]);
        for (const perm of normalized) {
            await client.query(
                'INSERT INTO namespace_permissions (actor_id, namespace, can_read, can_write, can_delete) VALUES ($1, $2, $3, $4, $5)',
                [actorId, perm.namespace, perm.can_read, perm.can_write, perm.can_delete]
            );
        }
        await client.query('COMMIT');
        clearPermissionsCache(actorId);
        logAdmin('permissions.save', { actor_id: actorId, count: normalized.length });
        res.json({ ok: true });
    } catch (err) {
        await client.query('ROLLBACK');
        console.error('Admin permissions save error:', err.message);
        res.status(500).json({ error: { code: 'INTERNAL', message: 'Failed to save permissions' } });
    } finally {
        client.release();
    }
});

// POST /admin/actors/visibility/read — get visibility grants for one actor
router.post('/admin/actors/visibility/read', async (req, res) => {
    const actorId = parseActorId(req.body.actor_id, res);
    if (actorId === null) return;
    try {
        // Verify actor exists
        const actor = await resolveById(actorId);
        if (!actor) {
            return res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Actor not found' } });
        }
        const result = await pool.query(
            `SELECT avc.target_actor_id, a.name AS target_name,
                    (ac.actor_id IS NOT NULL) AS target_is_agent,
                    (a.password_hash IS NOT NULL) AS target_is_user
             FROM actor_visibility_configuration avc
             LEFT JOIN actors a ON a.id = avc.target_actor_id
             LEFT JOIN agent_configuration ac ON ac.actor_id = a.id
             WHERE avc.actor_id = $1
             ORDER BY a.name NULLS FIRST`,
            [actorId]
        );
        // A row with target_actor_id = null means wildcard
        const hasWildcard = result.rows.some(r => r.target_actor_id === null);
        const grants = result.rows.filter(r => r.target_actor_id !== null);
        res.json({ wildcard: hasWildcard, grants });
    } catch (err) {
        console.error('Admin visibility read error:', err.message);
        res.status(500).json({ error: { code: 'INTERNAL', message: 'Failed to fetch visibility' } });
    }
});

// POST /admin/actors/visibility/save — full replace of visibility grants for one actor
// Body: { actor_id, wildcard: bool, grants: [actor_id, ...] }
router.post('/admin/actors/visibility/save', async (req, res) => {
    const actorId = parseActorId(req.body.actor_id, res);
    if (actorId === null) return;
    const { wildcard, grants } = req.body;
    if (typeof wildcard !== 'boolean' || !Array.isArray(grants)) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required fields: wildcard (bool), grants (array of actor IDs)' } });
    }

    // Verify actor exists
    const actor = await resolveById(actorId);
    if (!actor) {
        return res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Actor not found' } });
    }

    // Normalize grant IDs to integers, dedupe, exclude self
    const normalizedGrants = grants.map(g => parseInt(g, 10));
    if (normalizedGrants.some(g => !Number.isInteger(g) || g <= 0)) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Invalid grant actor ID' } });
    }
    const deduped = [...new Set(normalizedGrants)].filter(g => g !== actorId);

    // Verify all target actors exist
    if (deduped.length > 0) {
        const existCheck = await pool.query('SELECT id FROM actors WHERE id = ANY($1)', [deduped]);
        if (existCheck.rows.length !== deduped.length) {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'One or more target actors do not exist' } });
        }
    }

    const client = await pool.connect();
    try {
        await client.query('BEGIN');
        await client.query('DELETE FROM actor_visibility_configuration WHERE actor_id = $1', [actorId]);
        if (wildcard) {
            // Insert wildcard row (NULL target)
            await client.query(
                'INSERT INTO actor_visibility_configuration (actor_id, target_actor_id) VALUES ($1, NULL)',
                [actorId]
            );
        } else {
            for (const targetId of deduped) {
                await client.query(
                    'INSERT INTO actor_visibility_configuration (actor_id, target_actor_id) VALUES ($1, $2)',
                    [actorId, targetId]
                );
            }
        }
        await client.query('COMMIT');
        clearVisibilityCache(actorId);
        logAdmin('visibility.save', { actor_id: actorId, wildcard, count: deduped.length });
        res.json({ ok: true });
    } catch (err) {
        await client.query('ROLLBACK');
        console.error('Admin visibility save error:', err.message);
        res.status(500).json({ error: { code: 'INTERNAL', message: 'Failed to save visibility' } });
    } finally {
        client.release();
    }
});

// POST /admin/actors/namespaces — get distinct namespaces from documents (for dropdown)
router.post('/admin/actors/namespaces', async (req, res) => {
    try {
        const result = await pool.query(
            "SELECT DISTINCT namespace FROM documents WHERE namespace != '/' ORDER BY namespace"
        );
        res.json({ namespaces: result.rows.map(r => r.namespace) });
    } catch (err) {
        console.error('Admin namespaces error:', err.message);
        res.status(500).json({ error: { code: 'INTERNAL', message: 'Failed to fetch namespaces' } });
    }
});

module.exports = router;
