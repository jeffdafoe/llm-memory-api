const { Router } = require('express');
const crypto = require('crypto');
const generatePassphrase = require('eff-diceware-passphrase');
const pool = require('../db');
const { log } = require('../services/logger');
const auth = require('../middleware/auth');
const { hash: hashToken, generateSalt, verify } = require('../services/hashing');
const { SESSION_KIND } = require('../constants');
const { broadcast } = require('../services/events');
const { listNotes, readNote, saveNote } = require('../services/documents');
const sanitize = require('../sanitize');
const { requireAccess, validateNamespace } = require('../services/namespace-permissions');
const config = require('../services/config');
const { apiRoute } = require('../middleware/route-wrapper');
// actors service no longer needed — all routes use req.actorId from auth middleware

const router = Router();

// Agents with last_seen within this window are considered "online"
const ONLINE_THRESHOLD_MINUTES = 5;

// Session tokens expire after 24 hours
const SESSION_TTL_HOURS = 24;

// Passphrase rotation is suggested after 30 days
const ROTATION_THRESHOLD_DAYS = 30;

function logAgent(action, details) {
    log('agent', action, details);
}

// Dummy salt for timing-safe rejections — ensures agent-not-found paths
// take the same time as invalid-credential paths (prevents enumeration)
const DUMMY_SALT = generateSalt();

function generatePassphraseToken() {
    const words = generatePassphrase(3);
    return words.join('-');
}

// Generate a random session token (URL-safe base64, 48 bytes = 64 chars)
function generateSessionToken() {
    return crypto.randomBytes(48).toString('base64url');
}

// Agent registration is now handled via admin UI (POST /admin/agents/create).
// The old /agent/register and /agent/register/ack routes have been removed.

// POST /agent/login — verify passphrase, create session, return session token.
// No auth required — the passphrase in the body is the credential.
// Also cleans up expired sessions lazily on each call.
router.post('/agent/login', apiRoute('agent', 'login', async (req, res) => {
    const { passphrase } = req.body;
    const subsystem = sanitize.identifier(req.body.subsystem);
    const agent = sanitize.agentName(req.body.agent);

    if (!agent || !passphrase) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required fields: agent, passphrase' }
        });
    }

    // Look up actor and verify passphrase. JOIN agent_configuration to
    // enforce that this actor actually has agent capability — token_hash
    // alone isn't sufficient (a web-only user could theoretically have one).
    const actorResult = await pool.query(
        `SELECT a.id AS actor_id, a.name AS agent, a.token_hash, a.token_salt, a.status, a.passphrase_rotated_at
         FROM actors a
         JOIN agent_configuration agc ON agc.actor_id = a.id
         WHERE a.name = $1 AND a.token_hash IS NOT NULL`,
        [agent]
    );

    const row = actorResult.rows[0];

    // Compute hash even when row is missing (timing-safe rejection)
    if (!row) {
        hashToken(passphrase, DUMMY_SALT);
        logAgent('login-failed', { agent });
        return res.status(403).json({
            error: { code: 'INVALID_CREDENTIALS', message: 'Invalid agent or passphrase' }
        });
    }

    if (!verify(passphrase, row.token_salt, row.token_hash)) {
        logAgent('login-failed', { agent });
        return res.status(403).json({
            error: { code: 'INVALID_CREDENTIALS', message: 'Invalid agent or passphrase' }
        });
    }

    // Generate session token and store hashed in database
    const sessionToken = generateSessionToken();
    const sessionSalt = generateSalt();
    const sessionHash = hashToken(sessionToken, sessionSalt);
    const expiresAt = new Date(Date.now() + SESSION_TTL_HOURS * 60 * 60 * 1000);

    await pool.query(
        'INSERT INTO sessions (actor_id, token_hash, token_salt, kind, expires_at, subsystem) VALUES ($1, $2, $3, $4, $5, $6)',
        [row.actor_id, sessionHash, sessionSalt, SESSION_KIND.API, expiresAt, subsystem || null]
    );

    // Update last_seen
    await pool.query(
        'UPDATE actors SET last_seen = NOW() WHERE id = $1',
        [row.actor_id]
    );

    // Check if passphrase rotation is due
    let rotationDue = false;
    if (row.passphrase_rotated_at) {
        const daysSinceRotation = (Date.now() - new Date(row.passphrase_rotated_at).getTime()) / (1000 * 60 * 60 * 24);
        if (daysSinceRotation > ROTATION_THRESHOLD_DAYS) {
            rotationDue = true;
        }
    } else {
        // No rotation timestamp means never rotated — suggest rotation
        rotationDue = true;
    }

    logAgent('login', { agent, subsystem: subsystem || null });

    res.json({
        agent,
        session_token: sessionToken,
        expires_at: expiresAt,
        rotation_due: rotationDue
    });
}));

// POST /agent/logout — invalidate session token.
// Auth: session token (via middleware).
router.post('/agent/logout', apiRoute('agent', 'logout', async (req, res) => {
    const agent = req.authenticatedAgent;
    const token = req.headers.authorization.replace('Bearer ', '');

    // Find and delete the session matching this token
    const sessions = await pool.query(
        'SELECT id, token_hash, token_salt FROM sessions WHERE actor_id = $1 AND kind = $2',
        [req.actorId, SESSION_KIND.API]
    );

    let deleted = false;
    for (const row of sessions.rows) {
        if (verify(token, row.token_salt, row.token_hash)) {
            await pool.query('DELETE FROM sessions WHERE id = $1', [row.id]);
            deleted = true;
            break;
        }
    }

    // Clear from session cache
    auth.sessionCache.delete(token);

    logAgent('logout', { agent, session_deleted: deleted });

    res.json({
        agent,
        message: 'Logged out'
    });
}));

// POST /agent/rotate — generate new passphrase, invalidate all sessions.
// Auth: session token (via middleware) + current passphrase in body.
// Both are required — session proves identity, passphrase confirms intent.
router.post('/agent/rotate', apiRoute('agent', 'rotate', async (req, res) => {
    const agent = req.authenticatedAgent;
    const { current_passphrase } = req.body;

    if (!current_passphrase) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: current_passphrase' }
        });
    }

    // Verify current passphrase before allowing rotation (credentials on actors)
    const actorRow = await pool.query(
        'SELECT token_hash, token_salt FROM actors WHERE id = $1',
        [req.actorId]
    );
    if (!verify(current_passphrase, actorRow.rows[0].token_salt, actorRow.rows[0].token_hash)) {
        return res.status(403).json({
            error: { code: 'INVALID_PASSPHRASE', message: 'Current passphrase does not match' }
        });
    }

    // Generate new passphrase
    const passphrase = generatePassphraseToken();
    const salt = generateSalt();
    const hash = hashToken(passphrase, salt);

    await pool.query(
        'UPDATE actors SET token_hash = $1, token_salt = $2, passphrase_rotated_at = NOW() WHERE id = $3',
        [hash, salt, req.actorId]
    );

    // Invalidate all existing sessions for this agent
    await pool.query('DELETE FROM sessions WHERE actor_id = $1 AND kind = $2', [req.actorId, SESSION_KIND.API]);

    // Clear all cached sessions for this agent
    for (const [key, value] of auth.sessionCache.entries()) {
        if (value.agent === agent) {
            auth.sessionCache.delete(key);
        }
    }

    logAgent('rotate', { agent });

    res.json({
        agent,
        passphrase,
        message: 'Passphrase rotated. Save this — it will not be shown again. All sessions have been invalidated.'
    });
}));

// Explicit heartbeat — MCP servers call this on a 2-minute interval.
// The opportunistic heartbeat middleware also updates last_seen on every
// API call, so this is mainly a fallback for idle agents.
// Agent session required — admins cannot heartbeat on behalf of agents.
router.post('/agent/heartbeat', apiRoute('agent', 'heartbeat', async (req, res) => {
    const agent = req.authenticatedAgent;
    if (!agent) {
        return res.status(401).json({
            error: { code: 'UNAUTHORIZED', message: 'Agent session required' }
        });
    }

    const result = await pool.query(
        'UPDATE actors SET last_seen = NOW() WHERE id = $1 RETURNING last_seen',
        [req.actorId]
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
}));

// Returns all registered agents with online/offline status and per-agent
// unread counts (chat + mail) relative to the querying agent.
// Unread chat only counts the default channel (NULL) — not discussion channels.
// Unread counts use the authenticated caller's identity — agents see their own
// unread counts, admin users see zero (they don't have agent inboxes).
router.post('/agent/status', apiRoute('agent', 'status', async (req, res) => {
    // Use authenticated identity only — never trust req.body.agent
    const actorId = req.actorId;

    // Single query: join agent_status view with per-sender unread counts for
    // both chat and mail, so the caller sees everything at once.
    const result = await pool.query(
        `SELECT
            a.agent,
            a.status,
            a.last_seen,
            a.expertise,
            a.provider,
            a.model,
            a.virtual,
            a.active_since,
            COALESCE(c.unread_count, 0)::int AS unread_chat,
            COALESCE(m.unread_count, 0)::int AS unread_mail
        FROM agent_status a
        LEFT JOIN (
            SELECT fa.name AS from_agent, COUNT(*) AS unread_count
            FROM chat_messages cm
            JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
            JOIN actors fa ON fa.id = cmt.from_actor_id
            WHERE cm.to_actor_id = $1 AND cm.acked_at IS NULL AND cm.deleted_at IS NULL AND cmt.discussion_id IS NULL
            GROUP BY fa.name
        ) c ON c.from_agent = a.agent
        LEFT JOIN (
            SELECT fa.name AS from_agent, COUNT(*) AS unread_count
            FROM mail ml
            JOIN actors fa ON fa.id = ml.from_actor_id
            WHERE ml.to_actor_id = $1 AND ml.acked_at IS NULL AND ml.deleted_at IS NULL
            GROUP BY fa.name
        ) m ON m.from_agent = a.agent
        ORDER BY a.agent`,
        [actorId]
    );

    // Get active subsystems per agent from non-expired sessions
    const sessionsResult = await pool.query(
        `SELECT ac.name AS agent, s.subsystem
        FROM sessions s
        JOIN actors ac ON ac.id = s.actor_id
        WHERE s.kind = $1 AND s.expires_at > NOW() AND s.subsystem IS NOT NULL
        ORDER BY ac.name, s.subsystem`,
        [SESSION_KIND.API]
    );

    const subsystemsByAgent = {};
    for (const row of sessionsResult.rows) {
        if (!subsystemsByAgent[row.agent]) {
            subsystemsByAgent[row.agent] = [];
        }
        if (!subsystemsByAgent[row.agent].includes(row.subsystem)) {
            subsystemsByAgent[row.agent].push(row.subsystem);
        }
    }

    const agents = result.rows.map(row => ({
        agent: row.agent,
        status: row.status,
        last_seen: row.last_seen,
        expertise: row.expertise || [],
        provider: row.provider || null,
        model: row.model || null,
        active_since: row.active_since || null,
        subsystems: subsystemsByAgent[row.agent] || [],
        unread_chat: row.unread_chat,
        unread_mail: row.unread_mail
    }));

    res.json({ agents });
}));

// POST /agent/expertise — update the authenticated agent's expertise list.
// Auth: session token (via middleware).
// Body: { expertise: ["area1", "area2", ...] }
router.post('/agent/expertise', apiRoute('agent', 'expertise', async (req, res) => {
    const agent = req.authenticatedAgent;
    if (!agent) {
        return res.status(401).json({
            error: { code: 'UNAUTHORIZED', message: 'Agent session required' }
        });
    }

    const { expertise } = req.body;
    if (!Array.isArray(expertise)) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: expertise (array of strings)' }
        });
    }

    // Validate each entry is a non-empty string, sanitize content
    const cleaned = expertise
        .filter(e => typeof e === 'string' && e.trim().length > 0)
        .map(e => sanitize.content(e.trim().toLowerCase()));

    const json = JSON.stringify(cleaned);

    await pool.query(
        'UPDATE actors SET expertise = $1 WHERE id = $2',
        [json, req.actorId]
    );

    logAgent('expertise_update', { agent, expertise: cleaned });

    res.json({
        agent,
        expertise: cleaned,
        message: 'Expertise updated'
    });
}));

// POST /agent/profile — update the authenticated agent's provider and/or model.
// Auth: session token (via middleware).
// Body: { provider?: "anthropic", model?: "claude-4-sonnet" }
router.post('/agent/profile', apiRoute('agent', 'profile', async (req, res) => {
    const agent = req.authenticatedAgent;
    if (!agent) {
        return res.status(401).json({
            error: { code: 'UNAUTHORIZED', message: 'Agent session required' }
        });
    }

    const provider = req.body.provider !== undefined ? sanitize.identifier(req.body.provider) : undefined;
    const model = req.body.model !== undefined ? sanitize.identifier(req.body.model) : undefined;
    if (provider === undefined && model === undefined) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'At least one field required: provider, model' }
        });
    }

    const sets = [];
    const vals = [];
    let idx = 1;

    if (provider !== undefined) {
        sets.push(`provider = $${idx++}`);
        vals.push(provider);
    }
    if (model !== undefined) {
        sets.push(`model = $${idx++}`);
        vals.push(model);
    }
    vals.push(req.actorId);

    await pool.query(
        `UPDATE agent_configuration SET ${sets.join(', ')} WHERE actor_id = $${idx}`,
        vals
    );

    logAgent('profile_update', { agent, provider, model });

    res.json({
        agent,
        provider: provider !== undefined ? provider : undefined,
        model: model !== undefined ? model : undefined,
        message: 'Profile updated'
    });
}));

// POST /agent/activity/start — mark the authenticated agent as actively working.
// Auth: session token (via middleware).
router.post('/agent/activity/start', apiRoute('agent', 'activity-start', async (req, res) => {
    const agent = req.authenticatedAgent;
    if (!agent) {
        return res.status(401).json({
            error: { code: 'UNAUTHORIZED', message: 'Agent session required' }
        });
    }

    await pool.query(
        'UPDATE actors SET active_since = NOW() WHERE id = $1',
        [req.actorId]
    );

    logAgent('activity_start', { agent });
    broadcast('agent_activity', { agent, active: true });
    res.json({ agent, active: true, message: 'Activity started' });
}));

// POST /agent/activity/stop — mark the authenticated agent as idle.
// Auth: session token (via middleware).
router.post('/agent/activity/stop', apiRoute('agent', 'activity-stop', async (req, res) => {
    const agent = req.authenticatedAgent;
    if (!agent) {
        return res.status(401).json({
            error: { code: 'UNAUTHORIZED', message: 'Agent session required' }
        });
    }

    await pool.query(
        'UPDATE actors SET active_since = NULL WHERE id = $1',
        [req.actorId]
    );

    logAgent('activity_stop', { agent });
    broadcast('agent_activity', { agent, active: false });
    res.json({ agent, active: false, message: 'Activity stopped' });
}));

// POST /agent/instructions/read — read the authenticated agent's startup instructions.
// Auth: session token (via middleware).
router.post('/agent/instructions/read', apiRoute('agent', 'instructions-read', async (req, res) => {
    const agent = req.authenticatedAgent;
    if (!agent) {
        return res.status(401).json({
            error: { code: 'UNAUTHORIZED', message: 'Agent session required' }
        });
    }

    const result = await pool.query(
        'SELECT startup_instructions, dream_mode FROM agent_configuration WHERE actor_id = $1',
        [req.actorId]
    );

    let instructions = result.rows[0]?.startup_instructions || null;

    // Append dream bootstrap if agent has dreaming enabled
    if (result.rows[0]?.dream_mode && result.rows[0].dream_mode !== 'none') {
        const dreamBootstrap = config.get('dream_bootstrap') || '';
        if (dreamBootstrap) {
            instructions = (instructions || '') + '\n\n' + dreamBootstrap;
        }
    }

    // Append context/soul if it exists — the living soul document maintained by dream processing
    try {
        const soul = await readNote(agent, 'context/soul');
        if (soul && soul.content) {
            instructions = (instructions || '') + '\n\n' + soul.content;
        }
    } catch (e) {
        // Note doesn't exist yet — that's fine, skip silently
    }

    res.json({
        agent,
        instructions
    });
}));

// POST /agent/instructions/save — save startup instructions for the authenticated agent.
// Auth: session token (via middleware).
router.post('/agent/instructions/save', apiRoute('agent', 'instructions-save', async (req, res) => {
    const agent = req.authenticatedAgent;
    if (!agent) {
        return res.status(401).json({
            error: { code: 'UNAUTHORIZED', message: 'Agent session required' }
        });
    }

    const content = sanitize.content(req.body.content);
    if (content === undefined) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'Required field: content' }
        });
    }

    await pool.query(
        'UPDATE agent_configuration SET startup_instructions = $1 WHERE actor_id = $2',
        [content, req.actorId]
    );

    res.json({
        agent,
        message: 'Instructions saved',
        length: content ? content.length : 0
    });
}));

// ---------------------------------------------------------------------------
// POST /agent/memory/sync — bidirectional memory sync in a single round trip.
//
// The client sends its local file inventory (filename, content, mtime) and the
// remote prefix to sync against. The server compares each file against the
// corresponding remote note, saves locally-newer content to remote, and returns
// remotely-newer/remote-only content so the client can write it locally.
//
// Protocol is explicitly flat — filenames must be safe basenames (no slashes,
// no path traversal). Remote notes with nested slugs under the prefix are
// skipped with a warning in the log.
//
// Auth: agent session token (via middleware).
// Body: {
//   namespace: string (optional, defaults to agent name),
//   prefix: string (remote slug prefix, e.g. "instructions/memory/"),
//   files: [{ filename: string, content: string, mtime: ISO8601 string }]
// }
//
// Response: {
//   actions: [{
//     filename: string,
//     action: "pull" | "push" | "unchanged",
//     content?: string (included for "pull" actions),
//     title?: string (included for "pull" actions),
//     remote_updated_at?: string
//   }]
// }
// ---------------------------------------------------------------------------

// Validate that a filename is a safe flat basename — no slashes, no
// traversal, no empty strings. Returns true if safe.
function isSafeFilename(name) {
    if (!name || typeof name !== 'string') return false;
    if (name.includes('/') || name.includes('\\')) return false;
    if (name === '.' || name === '..') return false;
    if (name.startsWith('.')) return false;
    return true;
}

// Extract title from markdown frontmatter (name field) or fall back to filename
function extractTitle(content, filename) {
    if (content) {
        // Try frontmatter name field: ---\nname: Some Title\n---
        const fmMatch = content.match(/^---\s*\n[\s\S]*?name:\s*(.+)\n[\s\S]*?---/);
        if (fmMatch) {
            return fmMatch[1].trim();
        }
        // Try first markdown heading
        const headingMatch = content.match(/^#\s+(.+)/m);
        if (headingMatch) {
            return headingMatch[1].trim();
        }
    }
    // Fall back to filename without extension
    return filename.replace(/\.md$/, '').replace(/[_-]/g, ' ');
}

router.post('/agent/memory/sync', apiRoute('agent', 'memory-sync', async (req, res) => {
    const agent = req.authenticatedAgent;
    if (!agent) {
        return res.status(401).json({
            error: { code: 'UNAUTHORIZED', message: 'Agent session required' }
        });
    }

    // Server controls namespace and prefix — agent's own namespace, fixed prefix.
    const namespace = agent;
    const prefix = 'instructions/memory/';

    const response = {};

    // --- Memory sync ---
    // Bidirectional sync of memory files (local .md files ↔ remote notes).
    // Only runs if client sends a memory field.
    if (req.body.memory) {
        const localFiles = req.body.memory.files;

        if (!Array.isArray(localFiles)) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'memory.files must be an array of {filename, content, mtime}' }
            });
        }

        // Validate each file entry
        for (let i = 0; i < localFiles.length; i++) {
            const file = localFiles[i];
            if (!file || typeof file !== 'object') {
                return res.status(400).json({
                    error: { code: 'BAD_REQUEST', message: 'memory.files[' + i + '] is not an object' }
                });
            }
            if (!isSafeFilename(file.filename)) {
                return res.status(400).json({
                    error: { code: 'BAD_REQUEST', message: 'memory.files[' + i + '].filename is invalid: must be a safe basename (no slashes, no .., no leading dot)' }
                });
            }
            if (typeof file.content !== 'string') {
                return res.status(400).json({
                    error: { code: 'BAD_REQUEST', message: 'memory.files[' + i + '].content must be a string' }
                });
            }
            if (!file.mtime || isNaN(new Date(file.mtime).getTime())) {
                return res.status(400).json({
                    error: { code: 'BAD_REQUEST', message: 'memory.files[' + i + '].mtime must be a valid ISO8601 timestamp' }
                });
            }
        }

        await requireAccess(req.actorId, agent, 'agent', namespace, 'read');

        // Get all remote notes under the prefix
        const remoteData = await listNotes(namespace, 500, 0, prefix);
        const remoteNotes = remoteData.notes;

        // Build a map of remote notes by filename (slug minus prefix).
        // Only include notes that are direct children of the prefix (flat).
        // Normalize filenames: if the derived name has no file extension,
        // append .md so that remote slugs like "foo" and "foo.md" both
        // resolve to the local filename "foo.md".
        const remoteByFilename = {};
        for (const note of remoteNotes) {
            if (!note.slug.startsWith(prefix)) continue;
            let derived = note.slug.slice(prefix.length);
            // Skip nested slugs (contain /) — protocol is flat only
            if (!isSafeFilename(derived)) {
                continue;
            }
            // Normalize: if no file extension, append .md
            if (!derived.includes('.')) {
                derived = derived + '.md';
            }
            // Collision check: if two remote slugs normalize to the same
            // filename, keep the newer one and log a warning.
            if (remoteByFilename[derived]) {
                const existing = remoteByFilename[derived];
                const existingTime = new Date(existing.updated_at).getTime();
                const currentTime = new Date(note.updated_at).getTime();
                if (currentTime > existingTime) {
                    remoteByFilename[derived] = note;
                }
                continue;
            }
            remoteByFilename[derived] = note;
        }

        // Build a map of local files by filename
        const localByFilename = {};
        for (const file of localFiles) {
            localByFilename[file.filename] = file;
        }

        const allFilenames = new Set([
            ...Object.keys(remoteByFilename),
            ...Object.keys(localByFilename)
        ]);

        const actions = [];

        let hasWriteAccess = false;
        async function ensureWriteAccess() {
            if (!hasWriteAccess) {
                await requireAccess(req.actorId, agent, 'agent', namespace, 'write');
                hasWriteAccess = true;
            }
        }

        for (const filename of allFilenames) {
            const remote = remoteByFilename[filename];
            const local = localByFilename[filename];

            if (remote && !local) {
                const fullNote = await readNote(namespace, remote.slug);
                actions.push({
                    filename,
                    action: 'pull',
                    content: fullNote.content,
                    title: fullNote.title,
                    remote_updated_at: fullNote.updated_at
                });
            } else if (local && !remote) {
                await ensureWriteAccess();
                const slug = prefix + filename;
                const title = extractTitle(local.content, filename);
                const doc = await saveNote(namespace, title, local.content, slug, agent);
                actions.push({
                    filename,
                    action: 'push',
                    remote_updated_at: doc.updated_at
                });
            } else {
                const remoteTime = new Date(remote.updated_at).getTime();
                const localTime = new Date(local.mtime).getTime();

                if (remoteTime > localTime) {
                    const fullNote = await readNote(namespace, remote.slug);
                    actions.push({
                        filename,
                        action: 'pull',
                        content: fullNote.content,
                        title: fullNote.title,
                        remote_updated_at: fullNote.updated_at
                    });
                } else if (localTime > remoteTime) {
                    await ensureWriteAccess();
                    const slug = prefix + filename;
                    const title = extractTitle(local.content, filename);
                    const doc = await saveNote(namespace, title, local.content, slug, agent);
                    actions.push({
                        filename,
                        action: 'push',
                        remote_updated_at: doc.updated_at
                    });
                } else {
                    actions.push({ filename, action: 'unchanged' });
                }
            }
        }

        response.memory = { actions };
    }

    // --- Conversation sync ---
    // If client sent a conversations field (even empty object), return retention config.
    // This is independent of memory sync — errors here don't affect memory actions.
    if (req.body.conversations !== undefined && req.body.conversations !== null) {
        let conversationsResponse = {};
        try {
            const retentionDays = parseInt(config.get('conversation_retention_days')) || 30;
            conversationsResponse.retention_days = retentionDays;

            // Compare client sessions against existing conversation notes.
            // Supports two formats:
            //   - Legacy: session_ids = ["id1", "id2"] (no stale detection)
            //   - New:    sessions = [{id: "id1", file_size: 12345}, ...] (with stale detection)
            const sessions = req.body.conversations.sessions;
            const sessionIds = req.body.conversations.session_ids;
            const hasNewFormat = Array.isArray(sessions) && sessions.length > 0;
            const hasLegacyFormat = Array.isArray(sessionIds) && sessionIds.length > 0;

            if (hasNewFormat || hasLegacyFormat) {
                const uuidRegex = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
                const maxIds = 1000;

                // Normalize file_size to a bounded non-negative integer
                function normalizeFileSize(value) {
                    const n = Number(value);
                    return Number.isInteger(n) && n >= 0 ? n : 0;
                }

                // Normalize both formats into {id, file_size} pairs
                const rawItems = hasNewFormat
                    ? sessions
                        .filter(s => s && typeof s === 'object')
                        .map(s => ({ id: s.id, file_size: normalizeFileSize(s.file_size) }))
                    : sessionIds.map(id => ({ id, file_size: 0 }));

                if (rawItems.length > maxIds) {
                    conversationsResponse.error = 'Too many session IDs (max ' + maxIds + ')';
                } else {
                    const seen = new Set();
                    const validItems = [];
                    for (const item of rawItems) {
                        if (typeof item.id !== 'string' || !uuidRegex.test(item.id)) continue;
                        const lower = item.id.toLowerCase();
                        if (seen.has(lower)) continue;
                        seen.add(lower);
                        validItems.push({ id: lower, file_size: normalizeFileSize(item.file_size) });
                    }

                    const validIds = validItems.map(item => item.id);

                    // Query existing sessions with their stored file_size from metadata.
                    // Uses MAX to handle any duplicate rows per session, and regex guard
                    // on the cast to avoid blowing up on non-numeric stored values.
                    const existingResult = await pool.query(`
                        SELECT metadata->>'session_id' AS session_id,
                               MAX(
                                   CASE
                                       WHEN metadata->>'file_size' ~ '^[0-9]+$'
                                       THEN (metadata->>'file_size')::bigint
                                       ELSE 0
                                   END
                               ) AS file_size
                        FROM documents
                        WHERE namespace = $1 AND kind = 'conversation' AND deleted_at IS NULL
                          AND metadata->>'session_id' = ANY($2)
                        GROUP BY metadata->>'session_id'
                    `, [agent, validIds]);

                    const existingMap = new Map();
                    for (const row of existingResult.rows) {
                        existingMap.set(row.session_id.toLowerCase(), parseInt(row.file_size) || 0);
                    }

                    const missing = [];
                    const stale = [];
                    for (const item of validItems) {
                        if (!existingMap.has(item.id)) {
                            missing.push(item.id);
                        } else if (hasNewFormat && item.file_size > existingMap.get(item.id)) {
                            // File grew since last upload — session was extended
                            stale.push(item.id);
                        }
                    }

                    conversationsResponse.missing = missing;
                    if (stale.length > 0) {
                        conversationsResponse.stale = stale;
                    }
                }
            }

            // If client sent uploads, save them as conversation notes.
            // Server controls slug prefix and namespace.
            const uploads = req.body.conversations.uploads;
            if (Array.isArray(uploads) && uploads.length > 0) {
                let uploaded = 0;
                const uploadErrors = [];

                for (const upload of uploads) {
                    try {
                        if (!upload.content || !upload.session_id || !upload.date) {
                            uploadErrors.push({ session_id: upload.session_id || 'unknown', error: 'Missing required fields: content, session_id, date' });
                            continue;
                        }

                        const slug = 'conversations/' + upload.date + '-' + upload.session_id;
                        const title = 'Conversation ' + upload.date + ' (' + upload.session_id.slice(0, 8) + ')';

                        await saveNote(agent, title, upload.content, slug, agent, upload.metadata || undefined);
                        uploaded++;
                    } catch (uploadErr) {
                        uploadErrors.push({ session_id: upload.session_id || 'unknown', error: uploadErr.message });
                    }
                }

                conversationsResponse.uploaded = uploaded;
                if (uploadErrors.length > 0) {
                    conversationsResponse.upload_errors = uploadErrors;
                }
            }
        } catch (conversationErr) {
            conversationsResponse.error = 'Conversation sync failed: ' + conversationErr.message;
        }
        response.conversations = conversationsResponse;
    }

    res.json(response);
}));

// POST /agent/sync-mappings — list the authenticated agent's note sync mappings.
// Used by the sync script to discover which note prefixes to sync to which local paths.
router.post('/agent/sync-mappings', apiRoute('agent', 'sync-mappings', async (req, res) => {
    const agent = req.authenticatedAgent;
    if (!agent) {
        return res.status(401).json({
            error: { code: 'UNAUTHORIZED', message: 'Agent session required' }
        });
    }

    const result = await pool.query(`
        SELECT ns.id, ns.namespace, ns.slug, ns.local_path, ns.created_at
        FROM note_synchronization ns
        JOIN actors a ON a.id = ns.actor_id
        WHERE a.name = $1
        ORDER BY ns.namespace, ns.slug
    `, [agent]);
    // Include global exclude list so the sync script can skip matching slugs
    let excludeSlugs = [];
    try {
        const raw = config.get('sync_exclude_slugs');
        if (raw) {
            excludeSlugs = raw.split(',').map(s => s.trim()).filter(Boolean);
        }
    } catch (e) {
        // Key not configured — no exclusions
    }
    res.json({ mappings: result.rows, exclude_slugs: excludeSlugs });
}));

// POST /agent/config — return whitelisted config values.
// Agents don't need admin permissions to read these — they're operational
// parameters that tools like the discuss binary need at runtime.
// Add keys here as needed.
const AGENT_CONFIG_KEYS = new Set([
    'discussion_end_timeout_realtime',
    'discussion_maxrounds',
]);

router.post('/agent/config', apiRoute('agent', 'agent-config', async (req, res) => {
    const keys = [...AGENT_CONFIG_KEYS];
    const result = await pool.query(
        'SELECT key, value FROM config WHERE key = ANY($1)',
        [keys]
    );
    const config_values = {};
    for (const row of result.rows) {
        config_values[row.key] = row.value;
    }
    res.json({ config: config_values });
}));

module.exports = router;
