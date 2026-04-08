const { Router } = require('express');
const crypto = require('crypto');
const pool = require('../db');
const config = require('../services/config');
const { log } = require('../services/logger');
const { hash: hashToken, generateSalt, verify } = require('../services/hashing');
const { listNotes, readNote, saveNote, deleteNote, moveNote, validateSlug, escapeLike } = require('../services/documents');
const { searchMemory, ingestContent } = require('../services/memory');
const { getEntries: getRequestLogEntries } = require('../middleware/request-log');
const { getErrorLogEntries } = require('../services/logger');
const generatePassphrase = require('eff-diceware-passphrase');
const auth = require('../middleware/auth');
const { mailSend } = require('../services/mail');
const { chatSend } = require('../services/chat');
const { discussionCreate, discussionConclude } = require('../services/discussion');
const { formatPricing } = require('../services/provider');
const { resolveEffectiveLimits } = require('../services/virtual-agent');
const { requireByName, resolveByName, resolveById, checkNameAvailability, moderateActorName, clearCache: clearActorCache } = require('../services/actors');
const { hasAccess, requireAccess, getReadableNamespaces, validateNamespace, clearCache: clearPermissionsCache } = require('../services/namespace-permissions');
const { SESSION_KIND } = require('../constants');
const { getVisibleActorIds, canSee, clearCache: clearVisibilityCache } = require('../services/actor-visibility');
const { hasPermission, requirePerm, getPermissionMap, clearCache: clearAdminPermissionsCache } = require('../services/admin-permissions');
const notePerms = require('../services/note-permissions');
const { apiRoute } = require('../middleware/route-wrapper');
const sanitize = require('../sanitize');

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
// Check if the current user owns the target agent.
// Superadmins (*:*) bypass this check. Non-superadmins can only write
// to agents they created (actors.created_by = req.actorId) or themselves.
async function requireOwnership(req, res, targetActorId) {
    // Superadmin bypasses ownership check
    const isSuperAdmin = await hasPermission(req.actorId, '*', '*');
    if (isSuperAdmin) return true;

    // Users can always edit themselves
    if (targetActorId === req.actorId) return true;

    // Check created_by
    const result = await pool.query(
        'SELECT created_by FROM actors WHERE id = $1',
        [targetActorId]
    );
    if (result.rows.length > 0 && result.rows[0].created_by === req.actorId) {
        return true;
    }

    res.status(403).json({ error: { code: 'FORBIDDEN', message: 'You can only modify agents you created' } });
    return false;
}

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
    const username = sanitize.agentName(req.body.username);
    const { password } = req.body;

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

    let chatSql = `SELECT cm.id, fa.name AS from_agent, ta.name AS to_agent, cmt.discussion_id, cmt.message, cmt.sent_at, cm.acked_at
         FROM chat_messages cm
         JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
         JOIN actors fa ON fa.id = cmt.from_actor_id
         JOIN actors ta ON ta.id = cm.to_actor_id
         WHERE fa.name != 'system'
           AND cm.deleted_at IS NULL
           AND cmt.discussion_id IS NULL`;
    const chatParams = [];
    if (hasFilter) {
        chatSql += ' AND (cmt.from_actor_id = ANY($1) OR cm.to_actor_id = ANY($1))';
        chatParams.push(idsArray);
    }
    chatSql += ` ORDER BY CASE WHEN cm.acked_at IS NULL THEN 0 ELSE 1 END, cmt.sent_at DESC LIMIT 15`;
    const chat = await pool.query(chatSql, chatParams);

    let mailSql = `SELECT m.id, fa.name AS from_agent, ta.name AS to_agent, m.subject, m.body, m.sent_at, m.acked_at
         FROM mail m
         JOIN actors fa ON fa.id = m.from_actor_id
         JOIN actors ta ON ta.id = m.to_actor_id
         WHERE m.deleted_at IS NULL`;
    const mailParams = [];
    if (hasFilter) {
        mailSql += ' AND (m.from_actor_id = ANY($1) OR m.to_actor_id = ANY($1))';
        mailParams.push(idsArray);
    }
    mailSql += ` ORDER BY CASE WHEN m.acked_at IS NULL THEN 0 ELSE 1 END, m.sent_at DESC LIMIT 15`;
    const mail = await pool.query(mailSql, mailParams);

    let sysMsgSql = `SELECT cm.id, ta.name AS to_agent, cmt.discussion_id, cmt.message, cmt.sent_at, cm.acked_at
         FROM chat_messages cm
         JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
         JOIN actors fa ON fa.id = cmt.from_actor_id
         JOIN actors ta ON ta.id = cm.to_actor_id
         WHERE fa.name = 'system' AND cm.deleted_at IS NULL`;
    const sysMsgParams = [];
    if (hasFilter) {
        sysMsgSql += ' AND cm.to_actor_id = ANY($1)';
        sysMsgParams.push(idsArray);
    }
    sysMsgSql += ' ORDER BY cmt.sent_at DESC LIMIT 15';
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

    // Error count (last 24h), only 5xx — 4xx are expected client errors, not alarm-worthy.
    // status_code IS NULL covers legacy rows logged before the column was added.
    let errorCountSql = `SELECT COUNT(*) AS count FROM error_log WHERE created_at > NOW() - INTERVAL '24 hours' AND (status_code IS NULL OR status_code >= 500)`;
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
    // Subqueries for visibility and VA access summaries shown in the agent list
    let sql = `SELECT s.agent, s.actor_id, s.status, s.last_seen, s.passphrase_rotated_at, s.registered_at, s.provider, s.model, s.virtual, s.personality, s.active_since,
                s.cost_budget_daily, s.cost_budget_monthly, s.cache_prompts, s.learning_enabled, s.max_tokens, s.temperature, s.dream_mode, s.storage_quota, ac.configuration,
                COALESCE(vis.summary, 'self only') AS visibility_summary,
                va.summary AS va_access_summary
         FROM agent_status s
         LEFT JOIN agent_configuration ac ON ac.actor_id = s.actor_id
         LEFT JOIN LATERAL (
             SELECT CASE
                 WHEN EXISTS (SELECT 1 FROM actor_visibility_configuration WHERE actor_id = s.actor_id AND target_actor_id IS NULL)
                     THEN 'all agents'
                 WHEN (SELECT COUNT(*) FROM actor_visibility_configuration WHERE actor_id = s.actor_id AND target_actor_id IS NOT NULL) = 0
                     THEN 'self only'
                 ELSE (SELECT COUNT(*)::text || ' agent' || CASE WHEN COUNT(*) != 1 THEN 's' ELSE '' END
                        FROM actor_visibility_configuration WHERE actor_id = s.actor_id AND target_actor_id IS NOT NULL)
             END AS summary
         ) vis ON true
         LEFT JOIN LATERAL (
             SELECT CASE
                 WHEN NOT s.virtual THEN NULL
                 WHEN EXISTS (SELECT 1 FROM virtual_agent_access WHERE virtual_agent_id = s.actor_id AND grantee_actor_id IS NULL)
                     THEN 'public'
                 WHEN (SELECT COUNT(*) FROM virtual_agent_access WHERE virtual_agent_id = s.actor_id AND grantee_actor_id IS NOT NULL) = 0
                     THEN 'creator only'
                 ELSE (SELECT COUNT(*)::text || ' agent' || CASE WHEN COUNT(*) != 1 THEN 's' ELSE '' END
                        FROM virtual_agent_access WHERE virtual_agent_id = s.actor_id AND grantee_actor_id IS NOT NULL)
             END AS summary
         ) va ON true`;
    const params = [];
    if (visibleIds !== null) {
        sql += ' WHERE s.actor_id = ANY($1)';
        params.push(Array.from(visibleIds));
    }
    sql += ` ORDER BY CASE s.status WHEN 'online' THEN 0 WHEN 'available' THEN 1 WHEN 'offline' THEN 2 ELSE 3 END, s.last_seen DESC NULLS LAST`;
    const result = await pool.query(sql, params);

    // If any agent uses OpenRouter, pre-fetch the catalog so formatPricing
    // can show real pricing instead of a placeholder.
    if (result.rows.some(r => r.provider === 'openrouter')) {
        const { fetchCatalog } = require('../services/providers/openrouter');
        await fetchCatalog();
    }

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

    // Include global default storage quota so the frontend can display it
    const config = require('../services/config');
    const defaultStorageQuota = parseInt(config.get('default_storage_quota')) || 52428800;

    res.json({ agents: result.rows, default_storage_quota: defaultStorageQuota });
}));

// POST /admin/agents/instructions/read — read an agent's startup instructions
router.post('/admin/agents/instructions/read', requirePerm('agents', 'read'), adminRoute('agents-instructions-read', async (req, res) => {
    const agent = sanitize.agentName(req.body.agent);
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
    const agent = sanitize.agentName(req.body.agent);
    const { content } = req.body;
    if (!agent || content === undefined) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: agent, content' }
        });
    }
    const actor = await requireVisibility(req, res, agent);
    if (!actor) return;
    if (!await requireOwnership(req, res, actor.id)) return;
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
    const agent = sanitize.agentName(req.body.agent);
    const { expertise } = req.body;
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
    if (!await requireOwnership(req, res, actor.id)) return;
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
    const agent = sanitize.agentName(req.body.agent);
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
    if (!await requireOwnership(req, res, actor.id)) return;

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

// POST /admin/discussions/create — create a discussion from the admin UI
// The authenticated admin user is the creator and is auto-joined.
// Virtual agents are also auto-joined by the discussion service.
router.post('/admin/discussions/create', requirePerm('comms', 'write'), adminRoute('discussions-create', async (req, res) => {
    const topic = sanitize.content(req.body.topic || '').trim();
    const participants = Array.from(new Set((req.body.participants || []).filter(Boolean)));
    const mode = req.body.mode === 'async' ? 'async' : 'realtime';
    const context = req.body.context ? sanitize.content(req.body.context) : null;

    if (!topic || !Array.isArray(participants) || participants.length === 0) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: topic, participants (array of agent names)' }
        });
    }

    const createdBy = req.authenticatedUser.username;

    // Ensure creator is in participants list
    const allParticipants = participants.includes(createdBy)
        ? participants
        : [createdBy, ...participants];

    const result = await discussionCreate(topic, createdBy, allParticipants, [], null, mode, context);
    logAdmin('discussion_create', { topic, participants: allParticipants, mode, discussion_id: result.discussion_id, user_id: req.authenticatedUser.id });
    res.json(result);
}));

// POST /admin/discussions/send — send a message into a discussion as the admin user
router.post('/admin/discussions/send', requirePerm('comms', 'write'), adminRoute('discussions-send', async (req, res) => {
    const discussion_id = req.body.discussion_id;
    const message = sanitize.content(req.body.message || '').trim();

    if (!discussion_id || !message) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: discussion_id, message' }
        });
    }

    const fromAgent = req.authenticatedUser.username;
    const result = await chatSend(fromAgent, null, discussion_id, message, null);
    logAdmin('discussion_send', { discussion_id, from: fromAgent, user_id: req.authenticatedUser.id });
    res.json(result);
}));

// POST /admin/discussions/conclude — conclude a discussion from the admin UI
router.post('/admin/discussions/conclude', requirePerm('comms', 'write'), adminRoute('discussions-conclude', async (req, res) => {
    const { discussion_id } = req.body;

    if (!discussion_id) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: discussion_id' }
        });
    }

    const agent = req.authenticatedUser.username;
    const result = await discussionConclude(discussion_id, agent);
    logAdmin('discussion_conclude', { discussion_id, agent, user_id: req.authenticatedUser.id });
    res.json(result);
}));

// POST /admin/chat — list recent chat messages
router.post('/admin/chat', requirePerm('comms', 'read'), adminRoute('chat-list', async (req, res) => {
    const { limit = 50, discussion_id } = req.body;
    const visibleIds = await getVisibleActorIds(req.actorId);
    const hasFilter = visibleIds !== null;

    const discussionId = discussion_id ? parseInt(discussion_id, 10) : null;

    // For discussion channels, query distinct messages from chat_message_texts
    // to avoid showing duplicate delivery rows
    if (discussionId) {
        let sql = `SELECT cmt.id, fa.name AS from_agent, cmt.message, cmt.sent_at, cmt.discussion_id
                   FROM chat_message_texts cmt
                   JOIN actors fa ON fa.id = cmt.from_actor_id`;
        const conditions = ['cmt.discussion_id = $1'];
        const params = [discussionId];
        if (hasFilter) {
            params.push(Array.from(visibleIds));
            conditions.push('cmt.from_actor_id = ANY($' + params.length + ')');
        }
        sql += ' WHERE ' + conditions.join(' AND ');
        sql += ` ORDER BY cmt.sent_at DESC LIMIT $${params.length + 1}`;
        params.push(limit);

        const result = await pool.query(sql, params);
        res.json({ messages: result.rows });
    } else {
        // Non-discussion: query delivery rows as before
        let sql = `SELECT cm.id, fa.name AS from_agent, ta.name AS to_agent, cmt.message, cmt.sent_at, cm.acked_at
                   FROM chat_messages cm
                   JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
                   JOIN actors fa ON fa.id = cmt.from_actor_id
                   JOIN actors ta ON ta.id = cm.to_actor_id`;
        const conditions = ['cm.deleted_at IS NULL', 'cmt.discussion_id IS NULL'];
        const params = [];
        if (hasFilter) {
            params.push(Array.from(visibleIds));
            conditions.push('(cmt.from_actor_id = ANY($' + params.length + ') OR cm.to_actor_id = ANY($' + params.length + '))');
        }
        sql += ' WHERE ' + conditions.join(' AND ');
        sql += ` ORDER BY cmt.sent_at DESC LIMIT $${params.length + 1}`;
        params.push(limit);

        const result = await pool.query(sql, params);
        res.json({ messages: result.rows });
    }
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
        sql += ' WHERE (m.from_actor_id = ANY($1) OR m.to_actor_id = ANY($1))';
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
    // Scope delete to messages visible to this admin (sender or recipient must be visible)
    const visibleIds = await getVisibleActorIds(req.actorId);
    let sql = 'UPDATE mail SET deleted_at = NOW() WHERE id = $1 AND deleted_at IS NULL';
    const params = [id];
    if (visibleIds !== null) {
        const idsArray = Array.from(visibleIds);
        params.push(idsArray);
        sql += ' AND (from_actor_id = ANY($2) OR to_actor_id = ANY($2))';
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
    // Scope delete to messages visible to this admin (sender or recipient must be visible)
    const visibleIds = await getVisibleActorIds(req.actorId);
    let sql = 'UPDATE chat_messages SET deleted_at = NOW() WHERE id = $1 AND deleted_at IS NULL';
    const params = [id];
    if (visibleIds !== null) {
        const idsArray = Array.from(visibleIds);
        params.push(idsArray);
        sql += ` AND (to_actor_id = ANY($2) OR EXISTS (
            SELECT 1 FROM chat_message_texts cmt WHERE cmt.id = chat_messages.message_text_id AND cmt.from_actor_id = ANY($2)
        ))`;
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
    const to = sanitize.agentName(req.body.to);
    const subject = sanitize.content(req.body.subject);
    const body = sanitize.content(req.body.body);

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
// No permission check — static reference data, any authenticated user can read it
router.post('/admin/providers/registry', adminRoute('providers-registry', async (req, res) => {
    const { getRegistry } = require('../services/provider');
    res.json(getRegistry());
}));

// POST /admin/providers/defaults — get default configuration for a provider+model
router.post('/admin/providers/defaults', adminRoute('providers-defaults', async (req, res) => {
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

// POST /admin/providers/openrouter/models — lazily fetch full OpenRouter model catalog
router.post('/admin/providers/openrouter/models', adminRoute('openrouter-models', async (req, res) => {
    const { fetchCatalog } = require('../services/providers/openrouter');
    const catalog = await fetchCatalog();
    // Convert Map to array of { id, name, pricing } for the UI
    const models = [];
    for (const [id, entry] of catalog) {
        models.push({
            id,
            name: entry.name,
            pricing: { input: entry.input, output: entry.output, cache_read: entry.cache_read },
            context_length: entry.context_length
        });
    }
    res.json({ models });
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
    const nsAccess = await hasAccess(req.actorId, req.authenticatedUser.username, 'user', namespace, 'read');
    if (!nsAccess) {
        const noteAccess = await notePerms.hasNoteAccess(namespace, slug, req.actorId, 'read');
        if (!noteAccess) {
            return res.status(403).json({ error: { code: 'FORBIDDEN', message: 'Insufficient permissions' } });
        }
    }
    const note = await readNote(namespace, slug);
    res.json({ note });
}));

// POST /admin/notes/save — save (update) a note
router.post('/admin/notes/save', requirePerm('notes', 'write'), adminRoute('notes-save', async (req, res) => {
    const { namespace, extension } = req.body;
    const slug = sanitize.identifier(req.body.slug);
    const title = sanitize.content(req.body.title);
    const content = sanitize.content(req.body.content);
    if (!namespace || !slug || !title || content === undefined) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, slug, title, content' }
        });
    }
    validateNamespace(namespace);
    const nsAccess = await hasAccess(req.actorId, req.authenticatedUser.username, 'user', namespace, 'write');
    if (!nsAccess) {
        const noteAccess = await notePerms.hasNoteAccess(namespace, slug, req.actorId, 'write');
        if (!noteAccess) {
            return res.status(403).json({ error: { code: 'FORBIDDEN', message: 'Insufficient permissions' } });
        }
    }
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
    const nsAccess = await hasAccess(req.actorId, req.authenticatedUser.username, 'user', namespace, 'delete');
    if (!nsAccess) {
        const noteAccess = await notePerms.hasNoteAccess(namespace, slug, req.actorId, 'delete');
        if (!noteAccess) {
            return res.status(403).json({ error: { code: 'FORBIDDEN', message: 'Insufficient permissions' } });
        }
    }
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

// POST /admin/notes/move-prefix — rename a folder by updating all slugs with a given prefix.
// Supports dry_run mode to preview conflicts, and overwrite_slugs to soft-delete specific
// conflicting notes before moving.
router.post('/admin/notes/move-prefix', requirePerm('notes', 'write'), adminRoute('notes-move-prefix', async (req, res) => {
    const { namespace, old_prefix, new_prefix, dry_run, overwrite_slugs } = req.body;
    if (!namespace || !old_prefix || new_prefix == null) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: namespace, old_prefix, new_prefix' }
        });
    }
    validateNamespace(namespace);
    validateSlug(old_prefix, { allowTrailingSlash: true });
    if (new_prefix !== '') {
        validateSlug(new_prefix, { allowTrailingSlash: true });
    }
    await requireAccess(req.actorId, req.authenticatedUser.username, 'user', namespace, 'write');

    // Escape prefixes for use in LIKE patterns (backslash, %, _ are special)
    const oldLike = escapeLike(old_prefix);
    const newLike = escapeLike(new_prefix);

    // Count notes that would be moved
    const sourceNotes = await pool.query(`
        SELECT slug, title FROM documents
        WHERE namespace = $1 AND slug LIKE $2 || '%' AND deleted_at IS NULL
    `, [namespace, oldLike]);

    // Find actual conflicts: existing notes whose slug matches a computed destination slug.
    // Joins source notes (being moved) to destination notes where the renamed slug collides.
    const conflicts = await pool.query(`
        SELECT d2.slug, d2.title FROM documents d1
        JOIN documents d2
          ON d2.namespace = d1.namespace
          AND d2.slug = $3 || substring(d1.slug FROM length($2) + 1)
          AND d2.deleted_at IS NULL
        WHERE d1.namespace = $1
          AND d1.slug LIKE $4 || '%'
          AND d1.deleted_at IS NULL
        ORDER BY d2.slug
    `, [namespace, old_prefix, new_prefix, oldLike]);

    // Dry run: return what would happen without changing anything
    if (dry_run) {
        return res.json({
            dry_run: true,
            would_move: sourceNotes.rowCount,
            conflicts: conflicts.rows.map(r => ({ slug: r.slug, title: r.title }))
        });
    }

    // Determine which conflicts to overwrite vs skip
    const overwriteSet = new Set(overwrite_slugs || []);
    const toSkip = conflicts.rows.filter(r => !overwriteSet.has(r.slug));
    const toOverwrite = conflicts.rows.filter(r => overwriteSet.has(r.slug));

    // Soft-delete notes the user chose to overwrite
    if (toOverwrite.length > 0) {
        const overwriteSlugs = toOverwrite.map(r => r.slug);
        await pool.query(`
            UPDATE documents SET deleted_at = NOW()
            WHERE namespace = $1 AND slug = ANY($2) AND deleted_at IS NULL
        `, [namespace, overwriteSlugs]);
        // Also remove their vector chunks so they don't appear in search
        await pool.query(`
            DELETE FROM memory_chunks
            WHERE namespace = $1 AND source_file = ANY($2)
        `, [namespace, overwriteSlugs]);
    }

    // Build the list of slugs to skip (conflicts the user didn't choose to overwrite).
    // These stay in their old location — exclude them from the UPDATE.
    const skipSlugs = toSkip.map(r => {
        // Convert destination slug back to the source slug it would collide with
        return old_prefix + r.slug.substring(new_prefix.length);
    });

    // Update document slugs: replace the prefix portion, excluding skipped notes.
    // $2 = raw old_prefix (for length/substring), $3 = new_prefix (for concat),
    // $4 = escaped old_prefix (for LIKE)
    let docResult;
    if (skipSlugs.length > 0) {
        docResult = await pool.query(`
            UPDATE documents
            SET slug = $3 || substring(slug FROM length($2) + 1),
                updated_at = NOW()
            WHERE namespace = $1 AND slug LIKE $4 || '%' AND deleted_at IS NULL
              AND slug != ALL($5)
            RETURNING id, slug
        `, [namespace, old_prefix, new_prefix, oldLike, skipSlugs]);
    } else {
        docResult = await pool.query(`
            UPDATE documents
            SET slug = $3 || substring(slug FROM length($2) + 1),
                updated_at = NOW()
            WHERE namespace = $1 AND slug LIKE $4 || '%' AND deleted_at IS NULL
            RETURNING id, slug
        `, [namespace, old_prefix, new_prefix, oldLike]);
    }

    // Update vector chunks to match (same skip logic)
    if (skipSlugs.length > 0) {
        await pool.query(`
            UPDATE memory_chunks
            SET source_file = $3 || substring(source_file FROM length($2) + 1)
            WHERE namespace = $1 AND source_file LIKE $4 || '%'
              AND source_file != ALL($5)
        `, [namespace, old_prefix, new_prefix, oldLike, skipSlugs]);
    } else {
        await pool.query(`
            UPDATE memory_chunks
            SET source_file = $3 || substring(source_file FROM length($2) + 1)
            WHERE namespace = $1 AND source_file LIKE $4 || '%'
        `, [namespace, old_prefix, new_prefix, oldLike]);
    }

    // Update sync mappings that point to the old prefix
    await pool.query(`
        UPDATE note_synchronization
        SET slug = $3 || substring(slug FROM length($2) + 1)
        WHERE namespace = $1 AND slug LIKE $4 || '%'
    `, [namespace, old_prefix, new_prefix, oldLike]);

    logAdmin('note_move_prefix', {
        namespace, old_prefix, new_prefix,
        moved: docResult.rowCount, skipped: toSkip.length, overwritten: toOverwrite.length,
        user_id: req.authenticatedUser.id
    });
    res.json({ moved: docResult.rowCount, skipped: toSkip.length, overwritten: toOverwrite.length });
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
    let data = await searchMemory(query, targetNs, limit || 10, readable, req.actorId);
    res.json(data);
}));

// POST /admin/notes/namespaces — get list of namespaces with note counts
router.post('/admin/notes/namespaces', requirePerm('notes', 'read'), adminRoute('notes-namespaces', async (req, res) => {
    const result = await pool.query(
        `SELECT d.namespace, COUNT(*) AS count, COALESCE(nu.total_bytes, 0) AS total_bytes
         FROM documents d
         LEFT JOIN namespace_usage nu ON nu.namespace = d.namespace
         WHERE d.deleted_at IS NULL
         GROUP BY d.namespace, nu.total_bytes
         ORDER BY d.namespace`
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
                'SELECT namespace, slug, content, title, metadata FROM documents WHERE deleted_at IS NULL ORDER BY namespace, slug'
            );
            reindexState.total = docs.rows.length;

            // Check if enrichment should run alongside reindex
            const config = require('../services/config');
            const enrichEnabled = config.get('note_enrichment_enabled') === 'true';
            let enrichModule = null;
            if (enrichEnabled) {
                try {
                    enrichModule = require('../services/enrichment');
                } catch (err) {
                    // Enrichment module not available — skip silently
                }
            }
            const { autoExtractRelations } = require('../services/relations');

            for (const doc of docs.rows) {
                try {
                    const result = await ingestContent(doc.namespace, doc.slug, doc.content);
                    reindexState.chunks_created += result.chunks_created;

                    // Auto-extract slug references as relations
                    autoExtractRelations(doc.namespace, doc.slug, doc.content).catch(() => {});

                    // Fire-and-forget enrichment for each note (skips conversations/dreams internally)
                    if (enrichModule) {
                        var parsedMeta = doc.metadata ? (typeof doc.metadata === 'object' ? doc.metadata : JSON.parse(doc.metadata)) : null;
                        enrichModule.enrichNote(doc.namespace, doc.slug, doc.title, doc.content, parsedMeta).catch(() => {});
                        // Small delay to avoid hammering the enrichment provider
                        await new Promise(resolve => setTimeout(resolve, 200));
                    }
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

// POST /admin/notes/keyword-relations — generate "related" edges between notes
// that share keywords/tags. Pure SQL/JS, no LLM cost.
router.post('/admin/notes/keyword-relations', requirePerm('notes', 'write'), adminRoute('notes-keyword-relations', async (req, res) => {
    var minShared = req.body.min_shared !== undefined ? sanitize.positiveInt(req.body.min_shared, 1, 20) : undefined;
    if (req.body.min_shared !== undefined && minShared === null) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'min_shared must be an integer between 1 and 20' }
        });
    }
    const { generateKeywordRelations } = require('../services/enrichment');
    const result = await generateKeywordRelations(minShared);
    res.json(result);
}));

// POST /admin/notes/usage — namespace storage usage (note count + total bytes).
// Optionally filter by namespace. Joins actors table to show agent name.
router.post('/admin/notes/usage', requirePerm('notes', 'read'), adminRoute('notes-usage', async (req, res) => {
    const { namespace } = req.body;
    let sql, params;
    if (namespace) {
        sql = `
            SELECT nu.namespace, nu.note_count, nu.total_bytes, nu.updated_at,
                   ac.name AS agent
            FROM namespace_usage nu
            LEFT JOIN actors ac ON ac.name = nu.namespace
            WHERE nu.namespace = $1
            ORDER BY nu.namespace`;
        params = [namespace];
    } else {
        sql = `
            SELECT nu.namespace, nu.note_count, nu.total_bytes, nu.updated_at,
                   ac.name AS agent
            FROM namespace_usage nu
            LEFT JOIN actors ac ON ac.name = nu.namespace
            ORDER BY nu.total_bytes DESC`;
        params = [];
    }
    const result = await pool.query(sql, params);
    res.json({ usage: result.rows });
}));

// POST /admin/notes/relations — get relations for a note.
// Params: namespace, slug, direction (outgoing/incoming/both), type (optional).
// Filters to namespaces the user can read.
router.post('/admin/notes/relations', requirePerm('notes', 'read'), adminRoute('notes-relations', async (req, res) => {
    const { namespace, slug, direction, type } = req.body;
    if (!namespace || !slug) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required: namespace, slug' } });
    }
    const { getRelations } = require('../services/relations');
    const readable = await getReadableNamespaces(req.actorId, req.authenticatedUser.username, 'user');
    let relations = await getRelations(namespace, slug, direction || 'both', type);
    if (readable !== null) {
        relations = relations.filter(r => readable.includes(r.source_namespace) && readable.includes(r.target_namespace));
    }
    res.json({ relations });
}));

// POST /admin/notes/graph — graph traversal from a note.
// Params: namespace, slug, depth (1-5, default 2).
// Filters nodes/edges to readable namespaces.
router.post('/admin/notes/graph', requirePerm('notes', 'read'), adminRoute('notes-graph', async (req, res) => {
    const { namespace, slug, depth } = req.body;
    if (!namespace || !slug) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required: namespace, slug' } });
    }
    const { getGraph } = require('../services/relations');
    const readable = await getReadableNamespaces(req.actorId, req.authenticatedUser.username, 'user');
    const graph = await getGraph(namespace, slug, depth || 2);
    if (readable !== null) {
        graph.edges = graph.edges.filter(e => {
            const sNs = e.source.split('/')[0];
            const tNs = e.target.split('/')[0];
            return readable.includes(sNs) && readable.includes(tNs);
        });
        const reachable = new Set();
        for (const e of graph.edges) {
            reachable.add(e.source);
            reachable.add(e.target);
        }
        // Always keep root node
        const rootKey = namespace + '/' + slug;
        reachable.add(rootKey);
        graph.nodes = graph.nodes.filter(n => reachable.has(n.namespace + '/' + n.slug));
    }
    res.json(graph);
}));

// POST /admin/notes/graph-all — all relations, optionally filtered by namespace.
// For the graph overview mode. Filters to readable namespaces.
router.post('/admin/notes/graph-all', requirePerm('notes', 'read'), adminRoute('notes-graph-all', async (req, res) => {
    const { namespace } = req.body;
    const readable = await getReadableNamespaces(req.actorId, req.authenticatedUser.username, 'user');

    // Exclude noisy note kinds from the graph:
    // - conversation: raw session dumps
    // - context: system-managed docs (e.g. context/soul)
    // - instruction: bootstrap, instructions, GUIDELINES — infrastructure that everything references
    const excludeKinds = ['conversation', 'context', 'instruction'];

    let sql, params;
    if (namespace) {
        sql = `SELECT nr.id, nr.source_namespace, nr.source_slug, nr.target_namespace, nr.target_slug,
                      nr.relation_type, nr.auto_extracted, nr.created_at
               FROM note_relations nr
               LEFT JOIN documents sd ON sd.namespace = nr.source_namespace AND LOWER(sd.slug) = LOWER(nr.source_slug) AND sd.deleted_at IS NULL
               LEFT JOIN documents td ON td.namespace = nr.target_namespace AND LOWER(td.slug) = LOWER(nr.target_slug) AND td.deleted_at IS NULL
               WHERE (nr.source_namespace = $1 OR nr.target_namespace = $1)
                 AND (sd.kind IS NULL OR sd.kind != ALL($2))
                 AND (td.kind IS NULL OR td.kind != ALL($2))
               ORDER BY nr.created_at DESC`;
        params = [namespace, excludeKinds];
    } else {
        sql = `SELECT nr.id, nr.source_namespace, nr.source_slug, nr.target_namespace, nr.target_slug,
                      nr.relation_type, nr.auto_extracted, nr.created_at
               FROM note_relations nr
               LEFT JOIN documents sd ON sd.namespace = nr.source_namespace AND LOWER(sd.slug) = LOWER(nr.source_slug) AND sd.deleted_at IS NULL
               LEFT JOIN documents td ON td.namespace = nr.target_namespace AND LOWER(td.slug) = LOWER(nr.target_slug) AND td.deleted_at IS NULL
               WHERE (sd.kind IS NULL OR sd.kind != ALL($1))
                 AND (td.kind IS NULL OR td.kind != ALL($1))
               ORDER BY nr.created_at DESC`;
        params = [excludeKinds];
    }
    const result = await pool.query(sql, params);

    // Filter to readable namespaces
    let rows = result.rows;
    if (readable !== null) {
        rows = rows.filter(r => readable.includes(r.source_namespace) && readable.includes(r.target_namespace));
    }

    // Build nodes + edges
    const nodeSet = new Set();
    const nodes = [];
    const edges = [];
    for (const row of rows) {
        const sourceKey = row.source_namespace + '/' + row.source_slug;
        const targetKey = row.target_namespace + '/' + row.target_slug;
        if (!nodeSet.has(sourceKey)) {
            nodeSet.add(sourceKey);
            nodes.push({ namespace: row.source_namespace, slug: row.source_slug });
        }
        if (!nodeSet.has(targetKey)) {
            nodeSet.add(targetKey);
            nodes.push({ namespace: row.target_namespace, slug: row.target_slug });
        }
        edges.push({
            id: row.id,
            source: sourceKey,
            target: targetKey,
            type: row.relation_type,
            auto_extracted: row.auto_extracted
        });
    }
    res.json({ nodes, edges });
}));

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

const TEMPLATE_KINDS = new Set(['welcome', 'welcome-note']);

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
    const { id, kind } = req.body;
    const name = sanitize.content(req.body.name);
    const description = sanitize.content(req.body.description);
    const content = sanitize.content(req.body.content);
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
router.post('/admin/actors/create', requirePerm('agents', 'write'), adminRoute('actors-create', async (req, res) => {
    const { name, provider, model, welcome_template_id, welcome_note_template_id, virtual: isVirtual, personality,
            cost_budget_daily, cost_budget_monthly,
            cache_prompts, learning_enabled, max_tokens, temperature, configuration,
            ui_access, password, dream_mode } = req.body;

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

    // Check if actor name is available (existing actors + namespace collisions)
    const availability = await checkNameAvailability(actorName);
    if (!availability.available) {
        return res.status(409).json({
            error: { code: 'ALREADY_EXISTS', message: availability.reason }
        });
    }

    // Virtual agent moderation check (skips gracefully if VA not configured)
    const moderation = await moderateActorName(actorName);
    if (!moderation.approved) {
        return res.status(400).json({
            error: { code: 'NAME_REJECTED', message: moderation.reason }
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
            `INSERT INTO actors (name, token_hash, token_salt, password_hash, password_salt, status, created_by)
             VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
            [actorName, passphraseHash, passphraseSalt, passwordHash, passwordSalt, isVirtual === true ? 'available' : 'active', req.authenticatedUser.id]
        );
        actorId = actorResult.rows[0].id;

        // Create agent configuration
        await client.query(
            `INSERT INTO agent_configuration (actor_id, provider, model, virtual, personality, cost_budget_daily, cost_budget_monthly, cache_prompts, learning_enabled, max_tokens, temperature, configuration, dream_mode)
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
            [actorId, provider || null, model || null,
             isVirtual === true, personality || null,
             parseCostBudget(cost_budget_daily, 'cost_budget_daily'),
             parseCostBudget(cost_budget_monthly, 'cost_budget_monthly'),
             cache_prompts === true, learning_enabled !== false,
             max_tokens != null ? parseInt(max_tokens) : null,
             temperature != null ? parseFloat(temperature) : null,
             configuration ? JSON.stringify(configuration) : null,
             ['none', 'companion', 'technical'].includes(dream_mode) ? dream_mode : 'none']
        );

        // Grant all MCP permissions (full tool access) — non-virtual agents only
        if (!isVirtual) {
            await client.query(
                `INSERT INTO agent_permissions (actor_id, permission_id)
                 SELECT $1, id FROM permissions`,
                [actorId]
            );
        }

        // Grant the creator visibility to the new agent (so they can see it in the UI).
        // Skip if the creator already has wildcard visibility (sees everything).
        const creatorVis = await client.query(
            'SELECT target_actor_id FROM actor_visibility_configuration WHERE actor_id = $1 AND target_actor_id IS NULL',
            [req.actorId]
        );
        if (creatorVis.rows.length === 0) {
            await client.query(
                'INSERT INTO actor_visibility_configuration (actor_id, target_actor_id) VALUES ($1, $2) ON CONFLICT DO NOTHING',
                [req.actorId, actorId]
            );
        }

        await client.query('COMMIT');
        // Clear visibility cache so the new grant takes effect immediately
        const { clearCache: clearVisCache } = require('../services/actor-visibility');
        clearVisCache(req.actorId);
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

    // Apply welcome-note template if selected (non-virtual only)
    // Saves a getting-started note in the new agent's namespace
    let welcomeNoteSaved = false;
    if (welcome_note_template_id && !isVirtual) {
        try {
            const noteTplResult = await pool.query(
                'SELECT content FROM templates WHERE id = $1 AND kind = $2',
                [welcome_note_template_id, 'welcome-note']
            );
            if (noteTplResult.rows.length > 0) {
                const rawContent = noteTplResult.rows[0].content;
                const { frontmatter, body: tplBody } = parseTemplateFrontmatter(rawContent);
                const noteBody = tplBody.replace(/\{agent\}/g, actorName);
                const noteTitle = (frontmatter.title || 'Getting Started').replace(/\{agent\}/g, actorName);
                const noteSlug = frontmatter.slug || 'instructions/getting-started';
                await saveNote(actorName, noteTitle, noteBody, noteSlug, actorId);
                welcomeNoteSaved = true;
            }
        } catch (noteErr) {
            console.error('Post-commit welcome-note template error:', noteErr.message);
            if (!postCommitError) {
                postCommitError = 'Actor created but welcome note failed: ' + noteErr.message;
            }
        }
    }

    logAdmin('actor_create', { name: actorName, virtual: isVirtual === true, ui_access: !!ui_access, welcome_mail: welcomeMailSent, welcome_note: welcomeNoteSaved, user_id: req.authenticatedUser.id });

    const response = {
        name: actorName,
        passphrase,
        virtual: isVirtual === true,
        ui_access: !!ui_access,
        status: 'active',
        welcome_mail_sent: welcomeMailSent,
        welcome_note_saved: welcomeNoteSaved,
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
    const agent = sanitize.agentName(req.body.agent);
    const { timezone } = req.body;
    if (!agent) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required field: agent' } });
    }
    const actor = await requireVisibility(req, res, agent);
    if (!actor) return;
    const result = await pool.query(
        `SELECT ac.name AS agent, agc.provider, agc.model, agc.virtual, agc.personality, agc.configuration, ac.expertise,
                agc.cache_prompts, agc.learning_enabled, agc.max_tokens, agc.temperature,
                agc.cost_budget_daily, agc.cost_budget_monthly, agc.storage_quota,
                agc.api_key IS NOT NULL AS has_api_key,
                agc.dream_mode
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
        if (row.provider === 'openrouter') {
            const { fetchCatalog } = require('../services/providers/openrouter');
            await fetchCatalog();
        }
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
    const agent = sanitize.agentName(req.body.agent);
    const { personality, api_key, configuration, provider, model,
            cost_budget_daily, cost_budget_monthly, storage_quota,
            cache_prompts, learning_enabled, max_tokens, temperature, dream_mode } = req.body;

    if (!agent) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
        });
    }

    const actor = await requireVisibility(req, res, agent);
    if (!actor) return;
    if (!await requireOwnership(req, res, actor.id)) return;
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
    if (storage_quota !== undefined) {
        params.push(storage_quota === null || storage_quota === '' ? null : parseInt(storage_quota));
        updates.push(`storage_quota = $${idx++}`);
    }
    if (dream_mode !== undefined) {
        if (!['none', 'companion', 'technical'].includes(dream_mode)) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'dream_mode must be none, companion, or technical' }
            });
        }
        params.push(dream_mode);
        updates.push(`dream_mode = $${idx++}`);
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
    const agent = sanitize.agentName(req.body.agent);
    const { limit } = req.body;
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
router.post('/admin/actors/list', requirePerm('agents', 'read'), adminRoute('actors-list', async (req, res) => {
    const result = await pool.query(
        `SELECT a.id, a.name, a.created_at, a.visible_to_others, a.created_by,
                (ac.actor_id IS NOT NULL) AS is_agent,
                (a.password_hash IS NOT NULL) AS is_user,
                s.status, s.last_seen, s.registered_at,
                s.provider, s.model, s.virtual, s.personality,
                s.active_since, ac.configuration,
                creator.name AS created_by_name
         FROM actors a
         LEFT JOIN agent_configuration ac ON ac.actor_id = a.id
         LEFT JOIN agent_status s ON s.actor_id = a.id
         LEFT JOIN actors creator ON creator.id = a.created_by
         ORDER BY a.name`
    );

    // Pre-fetch OpenRouter catalog if any actor uses it
    if (result.rows.some(r => r.provider === 'openrouter')) {
        const { fetchCatalog } = require('../services/providers/openrouter');
        await fetchCatalog();
    }

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
router.post('/admin/actors/permissions/read', requirePerm('agents', 'read'), adminRoute('actors-permissions-read', async (req, res) => {
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
router.post('/admin/actors/permissions/save', requirePerm('agents', 'write'), adminRoute('actors-permissions-save', async (req, res) => {
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
router.post('/admin/actors/visibility/read', requirePerm('agents', 'read'), adminRoute('actors-visibility-read', async (req, res) => {
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
router.post('/admin/actors/visibility/save', requirePerm('agents', 'write'), adminRoute('actors-visibility-save', async (req, res) => {
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
router.post('/admin/actors/password', requirePerm('agents', 'write'), adminRoute('actors-password', async (req, res) => {
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
router.post('/admin/actors/namespaces', requirePerm('agents', 'read'), adminRoute('actors-namespaces', async (req, res) => {
    const result = await pool.query(
        "SELECT DISTINCT namespace FROM documents WHERE namespace != '/' ORDER BY namespace"
    );
    res.json({ namespaces: result.rows.map(r => r.namespace) });
}));

// Admin permission allowlist — server-side authority for valid resource/action pairs
const ADMIN_PERM_ALLOWLIST = {
    dashboard: ['read'],
    agents: ['read', 'write', 'create_system_equivalent'],
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
router.post('/admin/actors/admin-permissions/read', requirePerm('agents', 'read'), adminRoute('actors-admin-permissions-read', async (req, res) => {
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
router.post('/admin/actors/admin-permissions/save', requirePerm('agents', 'write'), adminRoute('actors-admin-permissions-save', async (req, res) => {
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

// POST /admin/actors/delete — permanently delete an actor and all associated data
router.post('/admin/actors/delete', requirePerm('agents', 'write'), adminRoute('actors-delete', async (req, res) => {
    const actorId = parseInt(req.body.actor_id);
    if (!Number.isInteger(actorId) || actorId <= 0) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required field: actor_id (positive integer)' } });
    }

    // Cannot delete yourself
    if (actorId === req.actorId) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Cannot delete your own account' } });
    }

    // Look up the actor
    const actorResult = await pool.query('SELECT id, name FROM actors WHERE id = $1', [actorId]);
    if (actorResult.rows.length === 0) {
        return res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Actor not found' } });
    }
    const actor = actorResult.rows[0];

    // Find virtual agents owned by this actor (created_by = actorId, virtual = true)
    const ownedVAs = await pool.query(
        `SELECT a.id, a.name FROM actors a
         JOIN agent_configuration ac ON ac.actor_id = a.id
         WHERE a.created_by = $1 AND ac.virtual = true`,
        [actorId]
    );
    const allActorIds = [actorId, ...ownedVAs.rows.map(r => r.id)];
    const allActorNames = [actor.name, ...ownedVAs.rows.map(r => r.name)];

    const client = await pool.connect();
    try {
        await client.query('BEGIN');

        // Delete in dependency order for all actor IDs (actor + their virtual agents)
        for (const id of allActorIds) {
            // Discussion system
            await client.query('DELETE FROM discussion_ballots WHERE actor_id = $1', [id]);
            await client.query('DELETE FROM discussion_votes WHERE proposed_by_actor_id = $1', [id]);
            await client.query('DELETE FROM discussion_participants WHERE actor_id = $1', [id]);

            // Chat & mail — delete delivery rows first (FK to chat_message_texts), then orphaned text rows
            await client.query('DELETE FROM chat_messages WHERE to_actor_id = $1', [id]);
            await client.query('DELETE FROM chat_messages WHERE message_text_id IN (SELECT id FROM chat_message_texts WHERE from_actor_id = $1)', [id]);
            await client.query('DELETE FROM chat_message_texts WHERE from_actor_id = $1', [id]);
            await client.query('DELETE FROM mail WHERE from_actor_id = $1 OR to_actor_id = $1', [id]);

            // Sessions & keys
            await client.query('DELETE FROM sessions WHERE actor_id = $1', [id]);
            await client.query('DELETE FROM mcp_sessions WHERE actor_id = $1', [id]);
            await client.query('DELETE FROM agent_api_keys WHERE actor_id = $1', [id]);

            // Permissions
            await client.query('DELETE FROM agent_permissions WHERE actor_id = $1', [id]);
            // These have ON DELETE CASCADE but explicit is clearer
            await client.query('DELETE FROM namespace_permissions WHERE actor_id = $1', [id]);
            await client.query('DELETE FROM admin_permissions WHERE actor_id = $1', [id]);
            await client.query('DELETE FROM actor_visibility_configuration WHERE actor_id = $1 OR target_actor_id = $1', [id]);
            await client.query('DELETE FROM note_permissions WHERE grantee_actor_id = $1 OR granted_by = $1', [id]);
            await client.query('DELETE FROM virtual_agent_access WHERE virtual_agent_id = $1 OR grantee_actor_id = $1', [id]);

            // Usage & sync
            await client.query('DELETE FROM virtual_agent_usage WHERE actor_id = $1', [id]);
            await client.query('DELETE FROM note_synchronization WHERE actor_id = $1', [id]);

            // Logs — SET NULL (keep log history, just detach the actor)
            await client.query('UPDATE system_errors SET actor_id = NULL WHERE actor_id = $1', [id]);
            await client.query('UPDATE error_log SET actor_id = NULL WHERE actor_id = $1', [id]);
            await client.query('UPDATE request_log SET actor_id = NULL WHERE actor_id = $1', [id]);

            // Detach documents created_by (before deleting namespace docs)
            await client.query('UPDATE documents SET created_by_actor_id = NULL WHERE created_by_actor_id = $1', [id]);
        }

        // Delete discussions created by any of the actors (after participants/votes/ballots are gone)
        for (const id of allActorIds) {
            await client.query('DELETE FROM discussions WHERE created_by_actor_id = $1', [id]);
        }

        // Delete documents and memory chunks by namespace (actor name = namespace)
        for (const name of allActorNames) {
            await client.query('DELETE FROM memory_chunks WHERE namespace = $1', [name]);
            await client.query('DELETE FROM documents WHERE namespace = $1', [name]);
        }

        // Clear created_by references from other actors pointing to these
        await client.query('UPDATE actors SET created_by = NULL WHERE created_by = ANY($1::int[])', [allActorIds]);

        // Delete agent_configuration rows (must come before actors due to FK)
        for (const id of allActorIds) {
            await client.query('DELETE FROM agent_configuration WHERE actor_id = $1', [id]);
        }

        // Delete the actors themselves (owned VAs first, then the main actor)
        for (const va of ownedVAs.rows) {
            await client.query('DELETE FROM actors WHERE id = $1', [va.id]);
        }
        await client.query('DELETE FROM actors WHERE id = $1', [actorId]);

        await client.query('COMMIT');

        // Clear caches so deleted actors don't linger
        clearActorCache();
        for (const id of allActorIds) {
            clearAdminPermissionsCache(id);
            clearVisibilityCache(id);
        }

        logAdmin('actor_delete', {
            actor_id: actorId,
            actor_name: actor.name,
            deleted_virtual_agents: ownedVAs.rows.map(r => r.name),
            user_id: req.authenticatedUser.id
        });

        res.json({
            message: 'Actor deleted',
            deleted: {
                actor: actor.name,
                virtual_agents: ownedVAs.rows.map(r => r.name)
            }
        });
    } catch (err) {
        await client.query('ROLLBACK');
        throw err;
    } finally {
        client.release();
    }
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
    const baseUrl = process.env.BASE_URL || (req.protocol + '://' + req.get('host'));
    result.rows.forEach(r => {
        if (r.invite_code) {
            r.register_url = baseUrl + '/register?code=' + r.invite_code;
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

    const registerUrl = (process.env.BASE_URL || (req.protocol + '://' + req.get('host'))) + '/register?code=' + code;
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

// ── Profile: visibility ───────────────────────────────────────────────────────

// POST /admin/profile/visibility — get or set visible_to_others
// Accepts optional actor_id for admin to manage other actors; defaults to self.
router.post('/admin/profile/visibility', adminRoute('profile-visibility', async (req, res) => {
    const targetId = req.body.actor_id || req.actorId;
    if (req.body.visible_to_others !== undefined) {
        await pool.query('UPDATE actors SET visible_to_others = $1 WHERE id = $2', [!!req.body.visible_to_others, targetId]);
        logAdmin('visibility_update', { actor_id: targetId, visible_to_others: !!req.body.visible_to_others, user_id: req.authenticatedUser.id });
        return res.json({ visible_to_others: !!req.body.visible_to_others });
    }
    const result = await pool.query('SELECT visible_to_others FROM actors WHERE id = $1', [targetId]);
    res.json({ visible_to_others: result.rows[0]?.visible_to_others || false });
}));

// ── Note Sharing ──────────────────────────────────────────────────────────────

// POST /admin/shares/create — create a note/folder share
router.post('/admin/shares/create', requirePerm('notes', 'write'), adminRoute('shares-create', async (req, res) => {
    const { owner_namespace, slug_pattern, grantee_actor_id, can_read, can_write, can_delete } = req.body;
    if (!owner_namespace || !slug_pattern) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required fields: owner_namespace, slug_pattern' } });
    }
    validateNamespace(owner_namespace);
    // Only the namespace owner (or admin with wildcard) can share
    await requireAccess(req.actorId, req.authenticatedUser.username, 'user', owner_namespace, 'write');

    const share = await notePerms.createShare({
        ownerNamespace: owner_namespace,
        slugPattern: slug_pattern,
        granteeActorId: grantee_actor_id !== undefined ? grantee_actor_id : null,
        canRead: can_read !== false,
        canWrite: !!can_write,
        canDelete: !!can_delete,
        grantedBy: req.actorId,
    });
    logAdmin('share_create', { owner_namespace, slug_pattern, grantee_actor_id, user_id: req.authenticatedUser.id });
    res.json({ share });
}));

// POST /admin/shares/revoke — revoke a share by ID
router.post('/admin/shares/revoke', requirePerm('notes', 'write'), adminRoute('shares-revoke', async (req, res) => {
    const { id } = req.body;
    if (!id) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required field: id' } });
    }
    // Look up the share to verify ownership
    const check = await pool.query('SELECT owner_namespace FROM note_permissions WHERE id = $1 AND revoked_at IS NULL', [id]);
    if (check.rows.length === 0) {
        return res.status(404).json({ error: { code: 'NOT_FOUND', message: 'Share not found' } });
    }
    await requireAccess(req.actorId, req.authenticatedUser.username, 'user', check.rows[0].owner_namespace, 'write');

    const share = await notePerms.revokeShare(id);
    logAdmin('share_revoke', { share_id: id, user_id: req.authenticatedUser.id });
    res.json({ share });
}));

// POST /admin/shares/list — list shares (by owner namespace, or all for admin)
router.post('/admin/shares/list', requirePerm('notes', 'read'), adminRoute('shares-list', async (req, res) => {
    const { owner_namespace, all } = req.body;

    if (all) {
        // Admin view: list all shares
        const shares = await notePerms.listAllShares();
        return res.json({ shares });
    }

    if (!owner_namespace) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required field: owner_namespace (or all: true)' } });
    }
    validateNamespace(owner_namespace);
    await requireAccess(req.actorId, req.authenticatedUser.username, 'user', owner_namespace, 'read');
    const shares = await notePerms.listSharesByOwner(owner_namespace);
    res.json({ shares });
}));

// POST /admin/shares/shared-with-me — list shares granted to the current user
router.post('/admin/shares/shared-with-me', requirePerm('notes', 'read'), adminRoute('shares-shared-with-me', async (req, res) => {
    const shares = await notePerms.listSharesForRecipient(req.actorId);
    res.json({ shares });
}));

// POST /admin/shares/documents — list actual documents shared with the current user
router.post('/admin/shares/documents', requirePerm('notes', 'read'), adminRoute('shares-documents', async (req, res) => {
    const docs = await notePerms.listSharedDocuments(req.actorId);
    // Group by namespace
    const byNamespace = {};
    for (const d of docs) {
        if (!byNamespace[d.namespace]) byNamespace[d.namespace] = [];
        byNamespace[d.namespace].push(d);
    }
    res.json({ namespaces: byNamespace });
}));

// POST /admin/shares/for-slug — get shares for a specific note (for the share dialog)
router.post('/admin/shares/for-slug', requirePerm('notes', 'read'), adminRoute('shares-for-slug', async (req, res) => {
    const { owner_namespace, slug_pattern } = req.body;
    if (!owner_namespace || !slug_pattern) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'Required fields: owner_namespace, slug_pattern' } });
    }
    validateNamespace(owner_namespace);
    await requireAccess(req.actorId, req.authenticatedUser.username, 'user', owner_namespace, 'read');

    const result = await pool.query(
        `SELECT np.*, a.name AS grantee_name
         FROM note_permissions np
         LEFT JOIN actors a ON np.grantee_actor_id = a.id
         WHERE np.owner_namespace = $1 AND np.slug_pattern = $2 AND np.revoked_at IS NULL
         ORDER BY np.grantee_actor_id`,
        [owner_namespace, slug_pattern]
    );
    res.json({ shares: result.rows });
}));

// POST /admin/actors/searchable — search actors for sharing (filtered by visible_to_others)
router.post('/admin/actors/searchable', requirePerm('notes', 'read'), adminRoute('actors-searchable', async (req, res) => {
    const { query } = req.body;
    const result = await pool.query(
        `SELECT id, name FROM actors
         WHERE visible_to_others = true AND name ILIKE $1 AND id != $2
         ORDER BY name LIMIT 20`,
        [`%${query || ''}%`, req.actorId]
    );
    res.json({ actors: result.rows });
}));

// POST /admin/virtual-agent-access/list — list access rules for virtual agents
router.post('/admin/virtual-agent-access/list', requirePerm('agents', 'read'), adminRoute('va-access-list', async (req, res) => {
    const result = await pool.query(`
        SELECT vaa.id, vaa.virtual_agent_id, va.name AS virtual_agent_name,
               vaa.grantee_actor_id, ga.name AS grantee_name, vaa.created_at
        FROM virtual_agent_access vaa
        JOIN actors va ON va.id = vaa.virtual_agent_id
        LEFT JOIN actors ga ON ga.id = vaa.grantee_actor_id
        ORDER BY va.name, ga.name NULLS FIRST
    `);
    res.json({ access: result.rows });
}));

// POST /admin/virtual-agent-access/grant — grant access to a virtual agent
router.post('/admin/virtual-agent-access/grant', requirePerm('agents', 'write'), adminRoute('va-access-grant', async (req, res) => {
    const { virtual_agent_id, grantee_actor_id } = req.body;
    if (!virtual_agent_id) return res.status(400).json({ error: 'virtual_agent_id required' });

    // Verify it's actually a virtual agent
    const vc = await pool.query('SELECT virtual FROM agent_configuration WHERE actor_id = $1', [virtual_agent_id]);
    if (!vc.rows[0]?.virtual) return res.status(400).json({ error: 'Not a virtual agent' });

    const result = await pool.query(
        'INSERT INTO virtual_agent_access (virtual_agent_id, grantee_actor_id) VALUES ($1, $2) ON CONFLICT DO NOTHING RETURNING id',
        [virtual_agent_id, grantee_actor_id || null]
    );
    logAdmin('va_access_grant', { virtual_agent_id, grantee_actor_id, user_id: req.authenticatedUser.id });
    res.json({ granted: result.rowCount > 0 });
}));

// POST /admin/virtual-agent-access/revoke — revoke access
router.post('/admin/virtual-agent-access/revoke', requirePerm('agents', 'write'), adminRoute('va-access-revoke', async (req, res) => {
    const { id } = req.body;
    if (!id) return res.status(400).json({ error: 'id required' });

    await pool.query('DELETE FROM virtual_agent_access WHERE id = $1', [id]);
    logAdmin('va_access_revoke', { access_id: id, user_id: req.authenticatedUser.id });
    res.json({ revoked: true });
}));

module.exports = router;
