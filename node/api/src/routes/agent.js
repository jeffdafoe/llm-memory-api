const { Router } = require('express');
const crypto = require('crypto');
const generatePassphrase = require('eff-diceware-passphrase');
const pool = require('../db');
const { log, logError } = require('../services/logger');
const auth = require('../middleware/auth');
const { hash: hashToken, generateSalt, verify } = require('../services/hashing');
const { SESSION_KIND } = require('../constants');
const { broadcast } = require('../services/events');
const { listNotes, readNote, saveNote } = require('../services/documents');
const { requireAccess, validateNamespace } = require('../services/namespace-permissions');
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
// The old /agent/register and /agent/register/ack routes have been removed
// since the CHECK constraint on actors.status only allows 'active'.

// POST /agent/login — verify passphrase, create session, return session token.
// No auth required — the passphrase in the body is the credential.
// Also cleans up expired sessions lazily on each call.
router.post('/agent/login', async (req, res) => {
    try {
        const { agent, passphrase, subsystem } = req.body;

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

        if (row.status !== 'active' || !verify(passphrase, row.token_salt, row.token_hash)) {
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
    } catch (err) {
        logError('agent', 'login', { agent: req.body.agent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: 'An internal error occurred' }
        });
    }
});

// POST /agent/logout — invalidate session token.
// Auth: session token (via middleware).
router.post('/agent/logout', async (req, res) => {
    try {
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
    } catch (err) {
        logError('agent', 'logout', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: 'An internal error occurred' }
        });
    }
});

// POST /agent/rotate — generate new passphrase, invalidate all sessions.
// Auth: session token (via middleware) + current passphrase in body.
// Both are required — session proves identity, passphrase confirms intent.
router.post('/agent/rotate', async (req, res) => {
    try {
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
    } catch (err) {
        logError('agent', 'rotate', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: 'An internal error occurred' }
        });
    }
});

// Explicit heartbeat — MCP servers call this on a 2-minute interval.
// The opportunistic heartbeat middleware also updates last_seen on every
// API call, so this is mainly a fallback for idle agents.
// Agent session required — admins cannot heartbeat on behalf of agents.
router.post('/agent/heartbeat', async (req, res) => {
    try {
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
    } catch (err) {
        logError('agent', 'heartbeat', { agent: req.body.agent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: 'An internal error occurred' }
        });
    }
});

// Returns all registered agents with online/offline status and per-agent
// unread counts (chat + mail) relative to the querying agent.
// Unread chat only counts the default channel (NULL) — not discussion channels.
// Unread counts use the authenticated caller's identity — agents see their own
// unread counts, admin users see zero (they don't have agent inboxes).
router.post('/agent/status', async (req, res) => {
    try {
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
                JOIN actors fa ON fa.id = cm.from_actor_id
                WHERE cm.to_actor_id = $1 AND cm.acked_at IS NULL AND cm.deleted_at IS NULL AND cm.channel IS NULL
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
            expertise: JSON.parse(row.expertise || '[]'),
            provider: row.provider || null,
            model: row.model || null,
            active_since: row.active_since || null,
            subsystems: subsystemsByAgent[row.agent] || [],
            unread_chat: row.unread_chat,
            unread_mail: row.unread_mail
        }));

        res.json({ agents });
    } catch (err) {
        logError('agent', 'status', { actorId: req.actorId, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: 'An internal error occurred' }
        });
    }
});

// POST /agent/expertise — update the authenticated agent's expertise list.
// Auth: session token (via middleware).
// Body: { expertise: ["area1", "area2", ...] }
router.post('/agent/expertise', async (req, res) => {
    try {
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

        // Validate each entry is a non-empty string
        const cleaned = expertise
            .filter(e => typeof e === 'string' && e.trim().length > 0)
            .map(e => e.trim().toLowerCase());

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
    } catch (err) {
        logError('agent', 'expertise', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: 'An internal error occurred' }
        });
    }
});

// POST /agent/profile — update the authenticated agent's provider and/or model.
// Auth: session token (via middleware).
// Body: { provider?: "anthropic", model?: "claude-4-sonnet" }
router.post('/agent/profile', async (req, res) => {
    try {
        const agent = req.authenticatedAgent;
        if (!agent) {
            return res.status(401).json({
                error: { code: 'UNAUTHORIZED', message: 'Agent session required' }
            });
        }

        const { provider, model } = req.body;
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
    } catch (err) {
        logError('agent', 'profile', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: 'An internal error occurred' }
        });
    }
});

// POST /agent/activity/start — mark the authenticated agent as actively working.
// Auth: session token (via middleware).
router.post('/agent/activity/start', async (req, res) => {
    try {
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
    } catch (err) {
        logError('agent', 'activity-start', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: 'An internal error occurred' }
        });
    }
});

// POST /agent/activity/stop — mark the authenticated agent as idle.
// Auth: session token (via middleware).
router.post('/agent/activity/stop', async (req, res) => {
    try {
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
    } catch (err) {
        logError('agent', 'activity-stop', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: 'An internal error occurred' }
        });
    }
});

// POST /agent/instructions/read — read the authenticated agent's startup instructions.
// Auth: session token (via middleware).
router.post('/agent/instructions/read', async (req, res) => {
    try {
        const agent = req.authenticatedAgent;
        if (!agent) {
            return res.status(401).json({
                error: { code: 'UNAUTHORIZED', message: 'Agent session required' }
            });
        }

        const result = await pool.query(
            'SELECT startup_instructions FROM agent_configuration WHERE actor_id = $1',
            [req.actorId]
        );

        res.json({
            agent,
            instructions: result.rows[0]?.startup_instructions || null
        });
    } catch (err) {
        logError('agent', 'instructions-read', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: 'An internal error occurred' }
        });
    }
});

// POST /agent/instructions/save — save startup instructions for the authenticated agent.
// Auth: session token (via middleware).
router.post('/agent/instructions/save', async (req, res) => {
    try {
        const agent = req.authenticatedAgent;
        if (!agent) {
            return res.status(401).json({
                error: { code: 'UNAUTHORIZED', message: 'Agent session required' }
            });
        }

        const { content } = req.body;
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
    } catch (err) {
        logError('agent', 'instructions-save', { agent: req.authenticatedAgent, message: err.message, detail: err.stack });
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: 'An internal error occurred' }
        });
    }
});

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

router.post('/agent/memory/sync', async (req, res) => {
    const agent = req.authenticatedAgent;
    if (!agent) {
        return res.status(401).json({
            error: { code: 'UNAUTHORIZED', message: 'Agent session required' }
        });
    }

    try {
        const namespace = req.body.namespace || agent;
        let prefix = req.body.prefix || '';
        const localFiles = req.body.files;

        // --- Input validation ---

        try {
            validateNamespace(namespace);
        } catch (err) {
            logError('agent', 'memory-sync-rejected', { agent, message: 'invalid namespace: ' + namespace });
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: err.message } });
        }

        // Normalize prefix: must be empty or end with /
        if (prefix && !prefix.endsWith('/')) {
            prefix += '/';
        }

        if (!Array.isArray(localFiles)) {
            logError('agent', 'memory-sync-rejected', { agent, message: 'files is not an array' });
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required field: files (array of {filename, content, mtime})' }
            });
        }

        // Validate each file entry
        for (let i = 0; i < localFiles.length; i++) {
            const file = localFiles[i];
            if (!file || typeof file !== 'object') {
                logError('agent', 'memory-sync-rejected', { agent, message: 'files[' + i + '] is not an object' });
                return res.status(400).json({
                    error: { code: 'BAD_REQUEST', message: 'files[' + i + '] is not an object' }
                });
            }
            if (!isSafeFilename(file.filename)) {
                logError('agent', 'memory-sync-rejected', { agent, message: 'unsafe filename: ' + file.filename, detail: 'index=' + i });
                return res.status(400).json({
                    error: { code: 'BAD_REQUEST', message: 'files[' + i + '].filename is invalid: must be a safe basename (no slashes, no .., no leading dot)' }
                });
            }
            if (typeof file.content !== 'string') {
                logError('agent', 'memory-sync-rejected', { agent, message: 'files[' + i + '].content must be a string', detail: 'filename=' + file.filename });
                return res.status(400).json({
                    error: { code: 'BAD_REQUEST', message: 'files[' + i + '].content must be a string' }
                });
            }
            if (!file.mtime || isNaN(new Date(file.mtime).getTime())) {
                logError('agent', 'memory-sync-rejected', { agent, message: 'invalid mtime for ' + file.filename + ': ' + file.mtime, detail: 'index=' + i });
                return res.status(400).json({
                    error: { code: 'BAD_REQUEST', message: 'files[' + i + '].mtime must be a valid ISO8601 timestamp' }
                });
            }
        }

        // --- Access control ---

        await requireAccess(req.actorId, agent, 'agent', namespace, 'read');

        // --- Build inventories ---

        // Get all remote notes under the prefix
        const remoteData = await listNotes(namespace, 500, 0, prefix || undefined);
        const remoteNotes = remoteData.notes;

        // Build a map of remote notes by filename (slug minus prefix).
        // Only include notes that are direct children of the prefix (flat).
        const remoteByFilename = {};
        for (const note of remoteNotes) {
            if (!note.slug.startsWith(prefix)) {
                // Shouldn't happen since listNotes filters by prefix, but be safe
                continue;
            }
            const derived = note.slug.slice(prefix.length);
            // Skip nested slugs (contain /) — protocol is flat only
            if (!isSafeFilename(derived)) {
                logError('agent', 'memory-sync-skip', { agent, message: 'nested or unsafe remote slug: ' + note.slug, detail: 'derived=' + derived });
                continue;
            }
            remoteByFilename[derived] = note;
        }

        // Build a map of local files by filename (already validated above)
        const localByFilename = {};
        for (const file of localFiles) {
            localByFilename[file.filename] = file;
        }

        // Collect all unique filenames from both sides
        const allFilenames = new Set([
            ...Object.keys(remoteByFilename),
            ...Object.keys(localByFilename)
        ]);

        const actions = [];

        // Check write access once if we might need to push
        let hasWriteAccess = false;
        async function ensureWriteAccess() {
            if (!hasWriteAccess) {
                await requireAccess(req.actorId, agent, 'agent', namespace, 'write');
                hasWriteAccess = true;
            }
        }

        // --- Compare and sync ---

        for (const filename of allFilenames) {
            const remote = remoteByFilename[filename];
            const local = localByFilename[filename];

            if (remote && !local) {
                // Remote-only — client needs to pull it down
                // Read full content (listNotes only returns snippet)
                const fullNote = await readNote(namespace, remote.slug);
                actions.push({
                    filename,
                    action: 'pull',
                    content: fullNote.content,
                    title: fullNote.title,
                    remote_updated_at: fullNote.updated_at
                });
            } else if (local && !remote) {
                // Local-only — push to remote
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
                // Both exist — compare timestamps
                const remoteTime = new Date(remote.updated_at).getTime();
                const localTime = new Date(local.mtime).getTime();

                if (remoteTime > localTime) {
                    // Remote is newer — pull
                    const fullNote = await readNote(namespace, remote.slug);
                    actions.push({
                        filename,
                        action: 'pull',
                        content: fullNote.content,
                        title: fullNote.title,
                        remote_updated_at: fullNote.updated_at
                    });
                } else if (localTime > remoteTime) {
                    // Local is newer — push
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
                    actions.push({
                        filename,
                        action: 'unchanged'
                    });
                }
            }
        }

        res.json({ actions });
    } catch (err) {
        if (err.statusCode === 400) {
            logError('agent', 'memory-sync-error', { agent, message: err.message });
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: err.message } });
        }
        if (err.statusCode === 403) {
            logError('agent', 'memory-sync-error', { agent, message: err.message });
            return res.status(403).json({ error: { code: 'FORBIDDEN', message: err.message } });
        }
        if (err.statusCode === 404) {
            logError('agent', 'memory-sync-error', { agent, message: err.message });
            return res.status(404).json({ error: { code: 'NOT_FOUND', message: err.message } });
        }
        logError('agent', 'memory-sync', { agent, message: err.message, detail: err.stack });
        res.status(500).json({ error: { code: 'INTERNAL_ERROR', message: 'Failed to sync notes' } });
    }
});

module.exports = router;
