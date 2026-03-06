const { Router } = require('express');
const crypto = require('crypto');
const generatePassphrase = require('eff-diceware-passphrase');
const pool = require('../db');
const { log } = require('../services/logger');
const auth = require('../middleware/auth');
const { hash: hashToken, generateSalt } = require('../services/hashing');

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
// since the CHECK constraint on agents.status only allows 'active'.

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

        // Look up agent and verify passphrase
        const agentResult = await pool.query(
            "SELECT agent, token_hash, token_salt, status, passphrase_rotated_at FROM agents WHERE agent = $1",
            [agent]
        );

        const row = agentResult.rows[0];
        const hash = hashToken(passphrase, row ? row.token_salt : DUMMY_SALT);

        if (!row || row.status !== 'active' || hash !== row.token_hash) {
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
            'INSERT INTO agent_sessions (agent, token_hash, token_salt, expires_at, subsystem) VALUES ($1, $2, $3, $4, $5)',
            [agent, sessionHash, sessionSalt, expiresAt, subsystem || null]
        );

        // Update last_seen
        await pool.query(
            'UPDATE agents SET last_seen = NOW() WHERE agent = $1',
            [agent]
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

        // Lazy cleanup: delete expired sessions
        await pool.query('DELETE FROM agent_sessions WHERE expires_at < NOW()');

        logAgent('login', { agent, subsystem: subsystem || null });

        res.json({
            agent,
            session_token: sessionToken,
            expires_at: expiresAt,
            rotation_due: rotationDue
        });
    } catch (err) {
        console.error('Agent login error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
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
            'SELECT id, token_hash, token_salt FROM agent_sessions WHERE agent = $1',
            [agent]
        );

        let deleted = false;
        for (const row of sessions.rows) {
            const hash = hashToken(token, row.token_salt);
            if (hash === row.token_hash) {
                await pool.query('DELETE FROM agent_sessions WHERE id = $1', [row.id]);
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
        console.error('Agent logout error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
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

        // Verify current passphrase before allowing rotation
        const agentRow = await pool.query(
            'SELECT token_hash, token_salt FROM agents WHERE agent = $1',
            [agent]
        );
        const currentHash = hashToken(current_passphrase, agentRow.rows[0].token_salt);
        if (currentHash !== agentRow.rows[0].token_hash) {
            return res.status(403).json({
                error: { code: 'INVALID_PASSPHRASE', message: 'Current passphrase does not match' }
            });
        }

        // Generate new passphrase
        const passphrase = generatePassphraseToken();
        const salt = generateSalt();
        const hash = hashToken(passphrase, salt);

        await pool.query(
            'UPDATE agents SET token_hash = $1, token_salt = $2, passphrase_rotated_at = NOW() WHERE agent = $3',
            [hash, salt, agent]
        );

        // Invalidate all existing sessions for this agent
        await pool.query('DELETE FROM agent_sessions WHERE agent = $1', [agent]);

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
        console.error('Agent rotate error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

// Explicit heartbeat — MCP servers call this on a 2-minute interval.
// The opportunistic heartbeat middleware also updates last_seen on every
// API call, so this is mainly a fallback for idle agents.
router.post('/agent/heartbeat', async (req, res) => {
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
        console.error('Agent heartbeat error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

// Returns all registered agents with online/offline status and per-agent
// unread counts (chat + mail) relative to the querying agent.
// Unread chat only counts the default channel (NULL) — not discussion channels.
router.post('/agent/status', async (req, res) => {
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
                a.status,
                a.last_seen,
                a.expertise,
                a.provider,
                a.model,
                COALESCE(c.unread_count, 0)::int AS unread_chat,
                COALESCE(m.unread_count, 0)::int AS unread_mail
            FROM agent_status a
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

        // Get active subsystems per agent from non-expired sessions
        const sessionsResult = await pool.query(
            `SELECT agent, subsystem
            FROM agent_sessions
            WHERE expires_at > NOW() AND subsystem IS NOT NULL
            ORDER BY agent, subsystem`
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
            subsystems: subsystemsByAgent[row.agent] || [],
            unread_chat: row.unread_chat,
            unread_mail: row.unread_mail
        }));

        res.json({ agents });
    } catch (err) {
        console.error('Agent status error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
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
            'UPDATE agents SET expertise = $1 WHERE agent = $2',
            [json, agent]
        );

        logAgent('expertise_update', { agent, expertise: cleaned });

        res.json({
            agent,
            expertise: cleaned,
            message: 'Expertise updated'
        });
    } catch (err) {
        console.error('Agent expertise update error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
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
        vals.push(agent);

        await pool.query(
            `UPDATE agents SET ${sets.join(', ')} WHERE agent = $${idx}`,
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
        console.error('Agent profile update error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
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
            'SELECT startup_instructions FROM agents WHERE agent = $1',
            [agent]
        );

        res.json({
            agent,
            instructions: result.rows[0]?.startup_instructions || null
        });
    } catch (err) {
        console.error('Instructions read error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
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
            'UPDATE agents SET startup_instructions = $1 WHERE agent = $2',
            [content, agent]
        );

        res.json({
            agent,
            message: 'Instructions saved',
            length: content ? content.length : 0
        });
    } catch (err) {
        console.error('Instructions save error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
