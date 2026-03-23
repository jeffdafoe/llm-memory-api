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
const { resolveEffectiveLimits } = require('../services/virtual-agent');
const { requireByName, resolveByName, resolveById } = require('../services/actors');
const { requireAccess, getReadableNamespaces, validateNamespace, clearCache: clearPermissionsCache } = require('../services/namespace-permissions');
const { SESSION_KIND } = require('../constants');
const { getVisibleActorIds, canSee, clearCache: clearVisibilityCache } = require('../services/actor-visibility');
const { requirePerm, getPermissionMap, clearCache: clearAdminPermissionsCache } = require('../services/admin-permissions');
const { apiRoute } = require('../middleware/route-wrapper');

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

function adminRoute(label, fn) {
    return apiRoute('admin', label, fn);
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

        const permissions = await getPermissionMap(row.id);

        res.json({
            session_token: sessionToken,
            expires_at: expiresAt.toISOString(),
            user: { id: row.id, username: row.username },
            permissions
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
router.post('/admin/dashboard', requirePerm('dashboard', 'read'), adminRoute('dashboard', async (req, res) => {
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
           AND cm.deleted_at IS NULL
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
         JOIN actors ta ON ta.id = m.to_actor_id
         WHERE m.deleted_at IS NULL`;
    const mailParams = [];
    if (hasFilter) {
        mailSql += ' AND m.from_actor_id = ANY($1) AND m.to_actor_id = ANY($1)';
        mailParams.push(idsArray);
    }
    mailSql += ` ORDER BY CASE WHEN m.acked_at IS NULL THEN 0 ELSE 1 END, m.sent_at DESC LIMIT 15`;
    const mail = await pool.query(mailSql, mailParams);

    let sysMsgSql = `SELECT cm.id, ta.name AS to_agent, cm.channel, cm.message, cm.sent_at, cm.acked_at
         FROM chat_messages cm
         JOIN actors fa ON fa.id = cm.from_actor_id
         JOIN actors ta ON ta.id = cm.to_actor_id
         WHERE fa.name = 'system' AND cm.deleted_at IS NULL`;
    const sysMsgParams = [];
    if (hasFilter) {
        sysMsgSql += ' AND cm.to_actor_id = ANY($1)';
        sysMsgParams.push(idsArray);
    }
    sysMsgSql += ' ORDER BY cm.sent_at DESC LIMIT 15';
    const systemMessages = await pool.query(sysMsgSql, sysMsgParams);

    // Note counts by namespace, filtered by readable namespaces
    const noteCountsResult = await pool.query(
        'SELECT namespace, COUNT(*) AS count FROM documents WHERE deleted_at IS NULL GROUP BY namespace ORDER BY namespace'
    );
    let noteCounts = noteCountsResult.rows;
    const readable = await getReadableNamespaces(req.actorId, req.authenticatedUser.username, 'user');
    if (readable !== null) {
        noteCounts = noteCounts.filter(r => readable.includes(r.namespace));
    }
    var noteTotal = 0;
    for (var nc of noteCounts) {
        noteTotal += parseInt(nc.count, 10);
    }

    // Error count (last 24h), filtered by agent visibility
    let errorCountSql = `SELECT COUNT(*) AS count FROM error_log WHERE created_at > NOW() - INTERVAL '24 hours'`;
    const errorCountParams = [];
    if (hasFilter) {
        errorCountSql += ' AND actor_id = ANY($1)';
        errorCountParams.push(idsArray);
    }
    const errorCountResult = await pool.query(errorCountSql, errorCountParams);
    const errorCount24h = parseInt(errorCountResult.rows[0].count, 10);

    res.json({
        agents: agents.rows,
        discussions: discussions.rows,
        chat: chat.rows,
        mail: mail.rows,
        system_messages: systemMessages.rows,
        notes: { total: noteTotal, namespaces: noteCounts.length },
        error_count_24h: errorCount24h
    });
}));

// POST /admin/api-log — recent API requests from request_log table
router.post('/admin/api-log', requirePerm('logs', 'read'), adminRoute('api-log', async (req, res) => {
    const { since_id, limit } = req.body;
    const visibleIds = await getVisibleActorIds(req.actorId);
    const entries = await getRequestLogEntries(since_id || 0, limit || 100, visibleIds);
    res.json({ entries });
}));

// POST /admin/error-log — recent errors from error_log table
router.post('/admin/error-log', requirePerm('logs', 'read'), adminRoute('error-log', async (req, res) => {
    const { since_id, limit } = req.body;
    const visibleIds = await getVisibleActorIds(req.actorId);
    const entries = await getErrorLogEntries(since_id || 0, limit || 100, visibleIds);
    res.json({ entries });
}));

// POST /admin/agents — list all agents
router.post('/admin/agents', requirePerm('agents', 'read'), adminRoute('agents-list', async (req, res) => {
    const visibleIds = await getVisibleActorIds(req.actorId);
    let sql = `SELECT s.agent, s.actor_id, s.status, s.last_seen, s.passphrase_rotated_at, s.registered_at, s.provider, s.model, s.virtual, s.personality, s.active_since,
                s.cost_budget_daily, s.cost_budget_monthly, s.cache_prompts, s.learning_enabled, s.max_tokens, s.temperature, ac.configuration
         FROM agent_status s
         LEFT JOIN agent_configuration ac ON ac.actor_id = s.actor_id`;
    const params = [];
    if (visibleIds !== null) {
        sql += ' WHERE s.actor_id = ANY($1)';
        params.push(Array.from(visibleIds));
    }
    sql += ` ORDER BY CASE s.status WHEN 'online' THEN 0 WHEN 'available' THEN 1 WHEN 'offline' THEN 2 ELSE 3 END, s.last_seen DESC NULLS LAST`;
    const result = await pool.query(sql, params);

    // Compute pricing_info for each agent via provider formatPricing
    for (const row of result.rows) {
        if (row.provider && row.model) {
            let config = {};
            if (row.configuration) {
                if (typeof row.configuration === 'object') {
                    config = row.configuration;
                } else {
                    try { config = JSON.parse(row.configuration); } catch (e) { /* ignore */ }
                }
            }
            row.pricing_info = formatPricing(row.provider, row.model, config);
        }
        delete row.configuration;
    }

    res.json({ agents: result.rows });
}));

// POST /admin/agents/instructions/read — read an agent's startup instructions
router.post('/admin/agents/instructions/read', requirePerm('agents', 'read'), adminRoute('agents-instructions-read', async (req, res) => {
    const { agent } = req.body;
    if (!agent) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
        });
    }
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
}));

// POST /admin/agents/instructions/save — save an agent's startup instructions
router.post('/admin/agents/instructions/save', requirePerm('agents', 'write'), adminRoute('agents-instructions-save', async (req, res) => {
    const { agent, content } = req.body;
    if (!agent || content === undefined) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: agent, content' }
        });
    }
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
}));

// POST /admin/agents/expertise/save — update an agent's expertise list
router.post('/admin/agents/expertise/save', requirePerm('agents', 'write'), adminRoute('agents-expertise-save', async (req, res) => {
    const { agent, expertise } = req.body;
    if (!agent || !Array.isArray(expertise)) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: agent, expertise (array of strings)' }
        });
    }
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
}));

// POST /admin/agents/reset-passphrase — generate new passphrase, invalidate all sessions
router.post('/admin/agents/reset-passphrase', requirePerm('agents', 'write'), adminRoute('agents-reset-passphrase', async (req, res) => {
    const { agent } = req.body;
    if (!agent) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
        });
    }
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
}));

// POST /admin/discussions — list discussions with optional status filter
router.post('/admin/discussions', requirePerm('comms', 'read'), adminRoute('discussions-list', async (req, res) => {
    const { status } = req.body;
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
}));

// POST /admin/discussions/detail — get full discussion details
router.post('/admin/discussions/detail', requirePerm('comms', 'read'), adminRoute('discussions-detail', async (req, res) => {
    const { discussion_id } = req.body;
    if (!discussion_id) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: discussion_id' }
        });
    }
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
}));

// POST /admin/chat — list recent chat messages
router.post('/admin/chat', requirePerm('comms', 'read'), adminRoute('chat-list', async (req, res) => {
    const { limit = 50, channel } = req.body;
    const visibleIds = await getVisibleActorIds(req.actorId);
    const hasFilter = visibleIds !== null;

    let sql = `SELECT cm.id, fa.name AS from_agent, ta.name AS to_agent, cm.channel, cm.message, cm.sent_at, cm.acked_at
               FROM chat_messages cm
               JOIN actors fa ON fa.id = cm.from_actor_id
               JOIN actors ta ON ta.id = cm.to_actor_id`;
    const conditions = ['cm.deleted_at IS NULL'];
    const params = [];
    if (channel) {
        params.push(channel);
        conditions.push('cm.channel = $' + params.length);
    }
    if (hasFilter) {
        params.push(Array.from(visibleIds));
        conditions.push('cm.from_actor_id = ANY($' + params.length + ') AND cm.to_actor_id = ANY($' + params.length + ')');
    }
    sql += ' WHERE ' + conditions.join(' AND ');
    sql += ` ORDER BY cm.sent_at DESC LIMIT $${params.length + 1}`;
    params.push(limit);

    const result = await pool.query(sql, params);
    res.json({ messages: result.rows });
}));

// POST /admin/mail — list mail
router.post('/admin/mail', requirePerm('comms', 'read'), adminRoute('mail-list', async (req, res) => {
    const { limit = 50 } = req.body;
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
}));

// POST /admin/mail/delete — soft-delete a mail message
router.post('/admin/mail/delete', requirePerm('comms', 'delete'), adminRoute('mail-delete', async (req, res) => {
    const { id } = req.body;
    if (!id) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: id' }
        });
    }
    // Scope delete to messages visible to this admin (both sender and recipient must be visible)
    const visibleIds = await getVisibleActorIds(req.actorId);
    let sql = 'UPDATE mail SET deleted_at = NOW() WHERE id = $1 AND deleted_at IS NULL';
    const params = [id];
    if (visibleIds !== null) {
        const idsArray = Array.from(visibleIds);
        params.push(idsArray);
        sql += ' AND from_actor_id = ANY($2) AND to_actor_id = ANY($2)';
    }
    sql += ' RETURNING id';
    const result = await pool.query(sql, params);
    if (result.rows.length === 0) {
        return res.status(404).json({
            error: { code: 'NOT_FOUND', message: 'Mail not found or already deleted' }
        });
    }
    logAdmin('mail_delete', { mail_id: id, user_id: req.authenticatedUser.id });
    res.json({ id, message: 'Mail deleted' });
}));

// POST /admin/chat/delete — soft-delete a chat message
router.post('/admin/chat/delete', requirePerm('comms', 'delete'), adminRoute('chat-delete', async (req, res) => {
    const { id } = req.body;
    if (!id) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: id' }
        });
    }
    // Scope delete to messages visible to this admin (both sender and recipient must be visible)
    const visibleIds = await getVisibleActorIds(req.actorId);
    let sql = 'UPDATE chat_messages SET deleted_at = NOW() WHERE id = $1 AND deleted_at IS NULL';
    const params = [id];
    if (visibleIds !== null) {
        const idsArray = Array.from(visibleIds);
        params.push(idsArray);
        sql += ' AND from_actor_id = ANY($2) AND to_actor_id = ANY($2)';
    }
    sql += ' RETURNING id';
    const result = await pool.query(sql, params);
    if (result.rows.length === 0) {
        return res.status(404).json({
            error: { code: 'NOT_FOUND', message: 'Chat message not found or already deleted' }
        });
    }
    logAdmin('chat_delete', { chat_id: id, user_id: req.authenticatedUser.id });
    res.json({ id, message: 'Chat message deleted' });
}));

// POST /admin/mail/send — send mail to an agent from the admin dashboard
router.post('/admin/mail/send', requirePerm('comms', 'write'), adminRoute('mail-send', async (req, res) => {
    const { to, subject, body } = req.body;

    if (!to || !subject || !body) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: to, subject, body' }
        });
    }

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
}));

// POST /admin/providers/registry — get provider/model registry for admin UI
router.post('/admin/providers/registry', requirePerm('config', 'read'), adminRoute('providers-registry', async (req, res) => {
    const { getRegistry } = require('../services/provider');
    res.json(getRegistry());
}));

// POST /admin/providers/defaults — get default configuration for a provider+model
router.post('/admin/providers/defaults', requirePerm('config', 'read'), adminRoute('providers-defaults', async (req, res) => {
    const { provider, model } = req.body;
    if (!provider || !model) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'provider and model are required' }
        });
    }
    const { getDefaultConfiguration } = require('../services/provider');
    const defaults = getDefaultConfiguration(provider, model);
    res.json({ defaults });
}));

// Config keys whose values must never be sent to the client
const SECRET_CONFIG_KEYS = new Set([
    'mcp_oauth_bearer_secret',
    'virtual_agent_encryption_key',
]);

// POST /admin/config/list — list all config key/value pairs
router.post('/admin/config/list', requirePerm('config', 'read'), adminRoute('config-list', async (req, res) => {
    const result = await pool.query('SELECT key, value, description FROM config ORDER BY key');
    const redacted = result.rows.map(row =>
        SECRET_CONFIG_KEYS.has(row.key)
            ? { ...row, value: '••••••••' }
            : row
    );
    res.json({ config: redacted });
}));

// POST /admin/config/update — update a config value by key
router.post('/admin/config/update', requirePerm('config', 'write'), adminRoute('config-update', async (req, res) => {
    const { key, value } = req.body;
    if (!key) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: key' }
        });
    }
    const result = await pool.query(
        'UPDATE config SET value = $1 WHERE key = $2',
        [value || '', key]
    );
    if (result.rowCount === 0) {
        return res.status(404).json({
            error: { code: 'NOT_FOUND', message: 'Config key not found' }
        });
    }
    // Update in-memory cache so changes take effect immediately
    config.set(key, value || '');
    logAdmin('config_update', { key });
    res.json({ success: true });
}));

// POST /admin/notes/list — list notes in a namespace
router.post('/admin/notes/list', requirePerm('notes', 'read'), adminRoute('notes-list', async (req, res) => {
    const { namespace, limit, offset, prefix } = req.body;
    if (!namespace) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: namespace' }
        });
    }
    validateNamespace(namespace);
    await requireAccess(req.actorId, req.authenticatedUser.username, 'user', namespace, 'read');
    const data = await listNotes(namespace, limit || 200, offset, prefix);
    res.json(data);
}));

// POST /admin/notes/read — read a single note
router.post('/admin/notes/read', requirePerm('notes', 'read'), adminRoute('notes-read', async (req, res) => {
    const { namespace, slug } = req.body;
    if (!namespace || !slug) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug' }
        });
    }
    validateNamespace(namespace);
    await requireAccess(req.actorId, req.authenticatedUser.username, 'user', namespace, 'read');
    const note = await readNote(namespace, slug);
    res.json({ note });
}));

// POST /admin/notes/save — save (update) a note
router.post('/admin/notes/save', requirePerm('notes', 'write'), adminRoute('notes-save', async (req, res) => {
    const { namespace, slug, title, content, extension } = req.body;
    if (!namespace || !slug || !title || content === undefined) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug, title, content' }
        });
    }
    validateNamespace(namespace);
    await requireAccess(req.actorId, req.authenticatedUser.username, 'user', namespace, 'write');
    const doc = await saveNote(namespace, title, content, slug, null, null, extension);
    logAdmin('note_save', { namespace, slug, user_id: req.authenticatedUser.id });
    res.json({ note: doc });
}));

// POST /admin/notes/delete — delete a note
router.post('/admin/notes/delete', requirePerm('notes', 'delete'), adminRoute('notes-delete', async (req, res) => {
    const { namespace, slug } = req.body;
    if (!namespace || !slug) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug' }
        });
    }
    validateNamespace(namespace);
    await requireAccess(req.actorId, req.authenticatedUser.username, 'user', namespace, 'delete');
    await deleteNote(namespace, slug);
    logAdmin('note_delete', { namespace, slug, user_id: req.authenticatedUser.id });
    res.json({ deleted: true, namespace, slug });
}));

// POST /admin/notes/move — rename a note's slug (and optionally namespace)
router.post('/admin/notes/move', requirePerm('notes', 'write'), adminRoute('notes-move', async (req, res) => {
    const { namespace, slug, new_slug, new_namespace } = req.body;
    if (!namespace || !slug || !new_slug) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug, new_slug' }
        });
    }
    validateNamespace(namespace);
    if (new_namespace) validateNamespace(new_namespace);
    await requireAccess(req.actorId, req.authenticatedUser.username, 'user', namespace, 'write');
    if (new_namespace && new_namespace !== namespace) {
        await requireAccess(req.actorId, req.authenticatedUser.username, 'user', new_namespace, 'write');
    }
    const doc = await moveNote(namespace, slug, new_slug, new_namespace);
    logAdmin('note_move', { namespace, slug, new_slug, new_namespace: new_namespace || namespace, user_id: req.authenticatedUser.id });
    res.json({ note: doc });
}));

// POST /admin/notes/search — semantic search across notes
router.post('/admin/notes/search', requirePerm('notes', 'read'), adminRoute('notes-search', async (req, res) => {
    const { query, namespace, limit } = req.body;
    if (!query) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: query' }
        });
    }
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
}));

// POST /admin/notes/namespaces — get list of namespaces with note counts
router.post('/admin/notes/namespaces', requirePerm('notes', 'read'), adminRoute('notes-namespaces', async (req, res) => {
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
}));

// In-memory reindex state — survives tab switches and page refreshes, cleared on server restart.
let reindexState = null; // { running, current, total, chunks_created, errors, result }

// POST /admin/notes/reindex — kick off background reindex, return immediately.
router.post('/admin/notes/reindex', requirePerm('notes', 'write'), adminRoute('notes-reindex', async (req, res) => {
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
}));

// POST /admin/notes/reindex-status — poll for reindex progress.
router.post('/admin/notes/reindex-status', requirePerm('notes', 'read'), (req, res) => {
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
router.post('/admin/notes/reindex-clear', requirePerm('notes', 'write'), (req, res) => {
    if (reindexState && !reindexState.running) {
        reindexState = null;
    }
    res.json({ ok: true });
});

// ---- Note Synchronization CRUD ----

// POST /admin/notes/sync/list — list sync mappings, optionally filtered by actor_id
router.post('/admin/notes/sync/list', requirePerm('notes', 'read'), adminRoute('sync-list', async (req, res) => {
    const { actor_id } = req.body;
    let query, params;
    if (actor_id) {
        query = `
            SELECT ns.id, ns.actor_id, a.name AS actor_name, ns.namespace, ns.slug, ns.local_path, ns.created_at
            FROM note_synchronization ns
            JOIN actors a ON a.id = ns.actor_id
            WHERE ns.actor_id = $1
            ORDER BY ns.namespace, ns.slug`;
        params = [actor_id];
    } else {
        query = `
            SELECT ns.id, ns.actor_id, a.name AS actor_name, ns.namespace, ns.slug, ns.local_path, ns.created_at
            FROM note_synchronization ns
            JOIN actors a ON a.id = ns.actor_id
            ORDER BY a.name, ns.namespace, ns.slug`;
        params = [];
    }
    const result = await pool.query(query, params);
    res.json({ mappings: result.rows });
}));

// POST /admin/notes/sync/save — create or update a sync mapping
router.post('/admin/notes/sync/save', requirePerm('notes', 'write'), adminRoute('sync-save', async (req, res) => {
    const { actor_id, namespace, slug, local_path } = req.body;
    if (!actor_id || !namespace || !local_path) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: actor_id, namespace, slug, local_path' }
        });
    }
    // slug can be empty string (entire namespace sync)
    const resolvedSlug = slug || '';
    // Verify actor exists
    const actor = await resolveById(actor_id);
    if (!actor) {
        return res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Actor not found' } });
    }
    const result = await pool.query(`
        INSERT INTO note_synchronization (actor_id, namespace, slug, local_path)
        VALUES ($1, $2, $3, $4)
        ON CONFLICT (actor_id, namespace, slug) DO UPDATE SET local_path = $4
        RETURNING *
    `, [actor_id, namespace, resolvedSlug, local_path]);
    res.json({ mapping: result.rows[0] });
}));

// POST /admin/notes/sync/delete — delete a sync mapping by id
router.post('/admin/notes/sync/delete', requirePerm('notes', 'write'), adminRoute('sync-delete', async (req, res) => {
    const { id } = req.body;
    if (!id) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: id' }
        });
    }
    await pool.query('DELETE FROM note_synchronization WHERE id = $1', [id]);
    res.json({ ok: true });
}));

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
router.post('/admin/templates/list', requirePerm('templates', 'read'), adminRoute('templates-list', async (req, res) => {
    const { kind } = req.body;
    if (kind && !TEMPLATE_KINDS.has(kind)) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Invalid kind: ' + kind }
        });
    }
    let query = 'SELECT id, name, kind, description, created_at, updated_at FROM templates';
    const params = [];
    if (kind) {
        query += ' WHERE kind = $1';
        params.push(kind);
    }
    query += ' ORDER BY name';
    const result = await pool.query(query, params);
    res.json({ templates: result.rows });
}));

// POST /admin/templates/read — read a single template
router.post('/admin/templates/read', requirePerm('templates', 'read'), adminRoute('templates-read', async (req, res) => {
    const { id } = req.body;
    if (!id) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: id' }
        });
    }
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
}));

// POST /admin/templates/save — create or update a template
router.post('/admin/templates/save', requirePerm('templates', 'write'), adminRoute('templates-save', async (req, res) => {
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
}));

// POST /admin/templates/delete — delete a template
router.post('/admin/templates/delete', requirePerm('templates', 'delete'), adminRoute('templates-delete', async (req, res) => {
    const { id } = req.body;
    if (!id) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: id' }
        });
    }
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
}));

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
router.post('/admin/actors/create', requirePerm('actors', 'write'), adminRoute('actors-create', async (req, res) => {
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
             VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
            [actorName, passphraseHash, passphraseSalt, passwordHash, passwordSalt, isVirtual === true ? 'available' : 'active']
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
}));

// POST /admin/agents/read — get full agent detail (includes configuration JSON)
// Accepts optional `timezone` (IANA string, e.g. "America/New_York") for local day boundary.
router.post('/admin/agents/read', requirePerm('agents', 'read'), adminRoute('agents-read', async (req, res) => {
    const { agent, timezone } = req.body;
    if (!agent) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required field: agent' } });
    }
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

    // Validate client timezone — untrusted input, fall back to UTC
    let tz = 'UTC';
    if (typeof timezone === 'string' && timezone) {
        try {
            Intl.DateTimeFormat(undefined, { timeZone: timezone });
            tz = timezone;
        } catch (e) {
            // Invalid timezone string, use UTC
        }
    }
    const costResult = await pool.query(
        `SELECT
            COALESCE(SUM(CASE WHEN created_at >= DATE_TRUNC('day', NOW() AT TIME ZONE $2) AT TIME ZONE $2 THEN cost ELSE 0 END), 0) AS cost_today,
            COALESCE(SUM(cost), 0) AS cost_monthly
         FROM virtual_agent_usage
         WHERE actor_id = $1 AND created_at >= NOW() - INTERVAL '30 days'`,
        [actor.id, tz]
    );
    row.cost_today = parseFloat(costResult.rows[0].cost_today);
    row.cost_monthly = parseFloat(costResult.rows[0].cost_monthly);

    // Resolve effective budget limits (agent override → system default → null)
    const { dailyLimit, monthlyLimit } = resolveEffectiveLimits(row);
    row.effective_daily_limit = dailyLimit;
    row.effective_monthly_limit = monthlyLimit;

    // Add pricing info string for display, accounting for agent's service tier
    if (row.provider && row.model) {
        let agentConfig = {};
        if (row.configuration) {
            if (typeof row.configuration === 'object') {
                agentConfig = row.configuration;
            } else {
                try { agentConfig = JSON.parse(row.configuration); } catch (e) { /* ignore */ }
            }
        }
        row.pricing_info = formatPricing(row.provider, row.model, agentConfig);
    }

    res.json(row);
}));

// POST /admin/agents/update — update virtual agent config
router.post('/admin/agents/update', requirePerm('agents', 'write'), adminRoute('agents-update', async (req, res) => {
    const { agent, personality, api_key, configuration, provider, model,
            cost_budget_daily, cost_budget_monthly,
            cache_prompts, learning_enabled, max_tokens, temperature } = req.body;

    if (!agent) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
        });
    }

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
}));

// POST /admin/agents/usage — get usage history for an agent
router.post('/admin/agents/usage', requirePerm('agents', 'read'), adminRoute('agents-usage', async (req, res) => {
    const { agent, limit } = req.body;
    if (!agent) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required field: agent' } });
    }
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
}));

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
router.post('/admin/actors/list', requirePerm('actors', 'read'), adminRoute('actors-list', async (req, res) => {
    const result = await pool.query(
        `SELECT a.id, a.name, a.created_at,
                (ac.actor_id IS NOT NULL) AS is_agent,
                (a.password_hash IS NOT NULL) AS is_user,
                s.status, s.last_seen, s.registered_at,
                s.provider, s.model, s.virtual, s.personality,
                s.active_since, ac.configuration
         FROM actors a
         LEFT JOIN agent_configuration ac ON ac.actor_id = a.id
         LEFT JOIN agent_status s ON s.actor_id = a.id
         ORDER BY a.name`
    );

    // Compute pricing_info for agents that have provider+model
    for (const row of result.rows) {
        if (row.provider && row.model) {
            let config = {};
            if (row.configuration) {
                if (typeof row.configuration === 'object') {
                    config = row.configuration;
                } else {
                    try { config = JSON.parse(row.configuration); } catch (e) { /* ignore */ }
                }
            }
            row.pricing_info = formatPricing(row.provider, row.model, config);
        }
        delete row.configuration;
    }

    res.json({ actors: result.rows });
}));

// POST /admin/actors/permissions/read — get namespace permissions for one actor
router.post('/admin/actors/permissions/read', requirePerm('actors', 'read'), adminRoute('actors-permissions-read', async (req, res) => {
    const actorId = parseActorId(req.body.actor_id, res);
    if (actorId === null) return;
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
}));

// POST /admin/actors/permissions/save — full replace of namespace permissions for one actor
// Body: { actor_id, permissions: [{ namespace, can_read, can_write, can_delete }] }
router.post('/admin/actors/permissions/save', requirePerm('actors', 'write'), adminRoute('actors-permissions-save', async (req, res) => {
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
    } catch (txErr) {
        await client.query('ROLLBACK');
        throw txErr;
    } finally {
        client.release();
    }
    clearPermissionsCache(actorId);
    logAdmin('permissions.save', { actor_id: actorId, count: normalized.length });
    res.json({ ok: true });
}));

// POST /admin/actors/visibility/read — get visibility grants for one actor
router.post('/admin/actors/visibility/read', requirePerm('actors', 'read'), adminRoute('actors-visibility-read', async (req, res) => {
    const actorId = parseActorId(req.body.actor_id, res);
    if (actorId === null) return;
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
}));

// POST /admin/actors/visibility/save — full replace of visibility grants for one actor
// Body: { actor_id, wildcard: bool, grants: [actor_id, ...] }
router.post('/admin/actors/visibility/save', requirePerm('actors', 'write'), adminRoute('actors-visibility-save', async (req, res) => {
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
    } catch (txErr) {
        await client.query('ROLLBACK');
        throw txErr;
    } finally {
        client.release();
    }
    clearVisibilityCache(actorId);
    logAdmin('visibility.save', { actor_id: actorId, wildcard, count: deduped.length });
    res.json({ ok: true });
}));

// POST /admin/actors/password — set or clear an actor's UI password
// Pass { actor_id, password: "string" } to set/change, or { actor_id, password: null } to clear
router.post('/admin/actors/password', requirePerm('actors', 'write'), adminRoute('actors-password', async (req, res) => {
    const { actor_id, password } = req.body;
    const actorId = parseActorId(actor_id, res);
    if (!actorId) return;

    const actor = await resolveById(actorId);
    if (!actor) {
        return res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Actor not found' } });
    }

    if (password === null || password === undefined) {
        // Clear password — remove UI access
        await pool.query(
            'UPDATE actors SET password_hash = NULL, password_salt = NULL WHERE id = $1',
            [actorId]
        );
        logAdmin('actor_password_clear', { actor_id: actorId, actor_name: actor.name, user_id: req.authenticatedUser.id });
        res.json({ message: 'UI access removed', is_user: false });
    } else {
        if (typeof password !== 'string' || password.length < 4) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Password must be at least 4 characters' }
            });
        }
        const salt = generateSalt();
        const hash = hashToken(password, salt);
        await pool.query(
            'UPDATE actors SET password_hash = $1, password_salt = $2 WHERE id = $3',
            [hash, salt, actorId]
        );
        logAdmin('actor_password_set', { actor_id: actorId, actor_name: actor.name, user_id: req.authenticatedUser.id });
        res.json({ message: 'Password updated', is_user: true });
    }
}));

// POST /admin/actors/namespaces — get distinct namespaces from documents (for dropdown)
router.post('/admin/actors/namespaces', requirePerm('actors', 'read'), adminRoute('actors-namespaces', async (req, res) => {
    const result = await pool.query(
        "SELECT DISTINCT namespace FROM documents WHERE namespace != '/' ORDER BY namespace"
    );
    res.json({ namespaces: result.rows.map(r => r.namespace) });
}));

// Admin permission allowlist — server-side authority for valid resource/action pairs
const ADMIN_PERM_ALLOWLIST = {
    dashboard: ['read'],
    agents: ['read', 'write'],
    comms: ['read', 'write', 'delete'],
    notes: ['read', 'write', 'delete'],
    config: ['read', 'write'],
    actors: ['read', 'write'],
    templates: ['read', 'write', 'delete'],
    logs: ['read']
};

function isValidAdminPerm(resource, action) {
    if (resource === '*' && action === '*') return true;
    const allowed = ADMIN_PERM_ALLOWLIST[resource];
    return allowed && allowed.includes(action);
}

// POST /admin/actors/admin-permissions/read — get admin permissions for an actor
router.post('/admin/actors/admin-permissions/read', requirePerm('actors', 'read'), adminRoute('actors-admin-permissions-read', async (req, res) => {
    const actorId = parseInt(req.body.actor_id);
    if (!Number.isInteger(actorId) || actorId <= 0) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required field: actor_id (positive integer)' } });
    }
    const result = await pool.query(
        'SELECT resource, action FROM admin_permissions WHERE actor_id = $1 ORDER BY resource, action',
        [actorId]
    );
    res.json({ permissions: result.rows });
}));

// POST /admin/actors/admin-permissions/save — replace all admin permissions for an actor
router.post('/admin/actors/admin-permissions/save', requirePerm('actors', 'write'), adminRoute('actors-admin-permissions-save', async (req, res) => {
    const actorId = parseInt(req.body.actor_id);
    const { permissions } = req.body;
    if (!Number.isInteger(actorId) || actorId <= 0 || !Array.isArray(permissions)) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required fields: actor_id (positive integer), permissions[]' } });
    }

    // Validate actor exists
    const actorCheck = await pool.query('SELECT id FROM actors WHERE id = $1', [actorId]);
    if (actorCheck.rows.length === 0) {
        return res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Actor not found' } });
    }

    // Validate and dedupe permissions against allowlist
    const validated = [];
    const seen = new Set();
    for (const perm of permissions) {
        const resource = String(perm.resource || '').trim();
        const action = String(perm.action || '').trim();
        if (!resource || !action) continue;
        if (!isValidAdminPerm(resource, action)) continue;
        const key = resource + ':' + action;
        if (seen.has(key)) continue;
        seen.add(key);
        validated.push({ resource, action });
    }

    // Delete existing permissions
    await pool.query('DELETE FROM admin_permissions WHERE actor_id = $1', [actorId]);

    // Insert validated permissions
    for (const perm of validated) {
        await pool.query(
            'INSERT INTO admin_permissions (actor_id, resource, action) VALUES ($1, $2, $3)',
            [actorId, perm.resource, perm.action]
        );
    }

    clearAdminPermissionsCache(actorId);
    logAdmin('admin_permissions_save', { actor_id: actorId, count: validated.length, user_id: req.authenticatedUser.id });

    // If editing own permissions, return updated map so frontend can refresh
    const response = { message: 'Admin permissions saved', count: validated.length };
    if (actorId === req.actorId) {
        response.updated_permissions = await getPermissionMap(actorId);
    }
    res.json(response);
}));

// ═══════════════════════════════════════════════════════════════════
//   Access Requests
// ═══════════════════════════════════════════════════════════════════

// POST /admin/access-requests — list access requests
router.post('/admin/access-requests', requirePerm('access', 'read'), adminRoute('access-requests-list', async (req, res) => {
    const { status } = req.body;
    let sql = `SELECT ar.id, ar.email, ar.usage_description, ar.status, ar.created_at, ar.reviewed_at, ar.reviewer_notes,
                       ic.code AS invite_code
                FROM access_requests ar
                LEFT JOIN invite_codes ic ON ic.access_request_id = ar.id`;
    const params = [];
    if (status) {
        sql += ' WHERE ar.status = $1';
        params.push(status);
    }
    sql += ' ORDER BY ar.created_at DESC';
    const result = await pool.query(sql, params);
    result.rows.forEach(r => {
        if (r.invite_code) {
            r.register_url = 'https://llm-memory.net/register?code=' + r.invite_code;
        }
    });
    res.json({ requests: result.rows });
}));

// POST /admin/access-requests/approve — approve request, generate invite code
router.post('/admin/access-requests/approve', requirePerm('access', 'write'), adminRoute('access-requests-approve', async (req, res) => {
    const { id, notes } = req.body;
    if (!id) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required field: id' } });
    }

    const request = await pool.query('SELECT id, status, email FROM access_requests WHERE id = $1', [id]);
    if (request.rows.length === 0) {
        return res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Access request not found' } });
    }
    if (request.rows[0].status !== 'pending') {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Request already ' + request.rows[0].status } });
    }

    const code = crypto.randomBytes(16).toString('hex');

    const client = await pool.connect();
    try {
        await client.query('BEGIN');
        await client.query(
            `UPDATE access_requests SET status = 'approved', reviewed_at = NOW(), reviewer_notes = $1 WHERE id = $2`,
            [notes || null, id]
        );
        await client.query(
            `INSERT INTO invite_codes (code, created_by, access_request_id, expires_at)
             VALUES ($1, $2, $3, NOW() + INTERVAL '30 days')`,
            [code, req.authenticatedUser.username, id]
        );
        await client.query('COMMIT');
    } catch (err) {
        await client.query('ROLLBACK');
        throw err;
    } finally {
        client.release();
    }

    const registerUrl = 'https://llm-memory.net/register?code=' + code;
    logAdmin('access_request_approved', { request_id: id, email: request.rows[0].email, user_id: req.authenticatedUser.id });
    res.json({ ok: true, invite_code: code, register_url: registerUrl, email: request.rows[0].email });
}));

// POST /admin/access-requests/reject — reject request
router.post('/admin/access-requests/reject', requirePerm('access', 'write'), adminRoute('access-requests-reject', async (req, res) => {
    const { id, notes } = req.body;
    if (!id) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required field: id' } });
    }

    const request = await pool.query('SELECT id, status FROM access_requests WHERE id = $1', [id]);
    if (request.rows.length === 0) {
        return res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Access request not found' } });
    }
    if (request.rows[0].status !== 'pending') {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Request already ' + request.rows[0].status } });
    }

    await pool.query(
        `UPDATE access_requests SET status = 'rejected', reviewed_at = NOW(), reviewer_notes = $1 WHERE id = $2`,
        [notes || null, id]
    );

    logAdmin('access_request_rejected', { request_id: id, user_id: req.authenticatedUser.id });
    res.json({ ok: true });
}));

// ═══════════════════════════════════════════════════════════════════
//   Invite Codes
// ═══════════════════════════════════════════════════════════════════

// POST /admin/invite-codes — list invite codes
router.post('/admin/invite-codes', requirePerm('access', 'read'), adminRoute('invite-codes-list', async (req, res) => {
    const result = await pool.query(
        `SELECT ic.id, ic.code, ic.created_at, ic.created_by, ic.used_by, ic.used_at, ic.expires_at,
                ar.email AS request_email
         FROM invite_codes ic
         LEFT JOIN access_requests ar ON ar.id = ic.access_request_id
         ORDER BY ic.created_at DESC`
    );
    res.json({ codes: result.rows });
}));

// POST /admin/invite-codes/generate — generate invite codes (batch)
router.post('/admin/invite-codes/generate', requirePerm('access', 'write'), adminRoute('invite-codes-generate', async (req, res) => {
    const { count = 1, expires_days } = req.body;
    const num = Math.min(Math.max(parseInt(count) || 1, 1), 50);
    const codes = [];

    for (let i = 0; i < num; i++) {
        const code = crypto.randomBytes(16).toString('hex');
        const expiresAt = expires_days ? `NOW() + INTERVAL '${parseInt(expires_days)} days'` : 'NULL';
        await pool.query(
            `INSERT INTO invite_codes (code, created_by, expires_at) VALUES ($1, $2, ${expiresAt})`,
            [code, req.authenticatedUser.username]
        );
        codes.push(code);
    }

    logAdmin('invite_codes_generated', { count: num, user_id: req.authenticatedUser.id });
    res.json({ ok: true, codes });
}));

// POST /admin/invite-codes/delete — delete an unused invite code
router.post('/admin/invite-codes/delete', requirePerm('access', 'write'), adminRoute('invite-codes-delete', async (req, res) => {
    const { id } = req.body;
    if (!id) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required field: id' } });
    }
    const code = await pool.query('SELECT id, used_by FROM invite_codes WHERE id = $1', [id]);
    if (code.rows.length === 0) {
        return res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Invite code not found' } });
    }
    if (code.rows[0].used_by) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Cannot delete a used invite code' } });
    }
    await pool.query('DELETE FROM invite_codes WHERE id = $1', [id]);
    logAdmin('invite_code_deleted', { code_id: id, user_id: req.authenticatedUser.id });
    res.json({ ok: true });
}));

module.exports = router;
