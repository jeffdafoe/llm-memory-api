const { Router } = require('express');
const crypto = require('crypto');
const fs = require('fs');
const path = require('path');
const generatePassphrase = require('eff-diceware-passphrase');
const pool = require('../db');
const { log } = require('../services/logger');
const auth = require('../middleware/auth');

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

function hashToken(plaintext, salt) {
    return crypto.pbkdf2Sync(plaintext, salt, 100000, 64, 'sha512').toString('hex');
}

function generatePassphraseToken() {
    const words = generatePassphrase(3);
    return words.join('-');
}

// Generate a random session token (URL-safe base64, 48 bytes = 64 chars)
function generateSessionToken() {
    return crypto.randomBytes(48).toString('base64url');
}

const ONBOARDING_PATH = path.join(__dirname, '..', '..', '..', '..', 'templates', 'onboarding.md');

function buildOnboarding(agent) {
    try {
        const template = fs.readFileSync(ONBOARDING_PATH, 'utf-8');
        return template.replace(/\{\{agent\}\}/g, agent);
    } catch (err) {
        console.error('Failed to read onboarding template:', err.message);
        return null;
    }
}

// POST /agent/register — create agent, generate token, return plaintext
router.post('/agent/register', async (req, res) => {
    try {
        const { agent } = req.body;

        if (!agent) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required field: agent' }
            });
        }

        // Check if agent already exists
        const existing = await pool.query(
            'SELECT agent, status FROM agents WHERE agent = $1',
            [agent]
        );

        if (existing.rows.length > 0) {
            const row = existing.rows[0];
            if (row.status === 'active') {
                return res.status(409).json({
                    error: { code: 'ALREADY_REGISTERED', message: 'Agent is already registered and active' }
                });
            }
            // Status is 'pending' — regenerate token (they haven't acked yet)
            const token = generatePassphraseToken();
            const salt = crypto.randomBytes(32).toString('hex');
            const hash = hashToken(token, salt);

            await pool.query(
                'UPDATE agents SET token_hash = $1, token_salt = $2, status = $3 WHERE agent = $4',
                [hash, salt, 'pending', agent]
            );

            logAgent('re-register', { agent });

            return res.json({
                agent,
                token,
                status: 'pending',
                message: 'Token regenerated. Call POST /agent/register/ack with your agent name and token to activate.',
                onboarding: buildOnboarding(agent)
            });
        }

        // New agent
        const token = generatePassphraseToken();
        const salt = crypto.randomBytes(32).toString('hex');
        const hash = hashToken(token, salt);

        await pool.query(
            'INSERT INTO agents (agent, token_hash, token_salt, status) VALUES ($1, $2, $3, $4)',
            [agent, hash, salt, 'pending']
        );

        logAgent('register', { agent });

        res.json({
            agent,
            token,
            status: 'pending',
            message: 'Save this token — it will not be shown again. Call POST /agent/register/ack with your agent name and token to activate.',
            onboarding: buildOnboarding(agent)
        });
    } catch (err) {
        console.error('Agent register error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

// POST /agent/register/ack — validate token, activate agent
router.post('/agent/register/ack', async (req, res) => {
    try {
        const { agent, token } = req.body;

        if (!agent || !token) {
            return res.status(400).json({
                error: { code: 'BAD_REQUEST', message: 'Required fields: agent, token' }
            });
        }

        const result = await pool.query(
            'SELECT token_hash, token_salt, status FROM agents WHERE agent = $1',
            [agent]
        );

        if (result.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Agent not found' }
            });
        }

        const row = result.rows[0];

        if (row.status === 'active') {
            return res.status(409).json({
                error: { code: 'ALREADY_ACTIVE', message: 'Agent is already active' }
            });
        }

        const hash = hashToken(token, row.token_salt);
        if (hash !== row.token_hash) {
            return res.status(403).json({
                error: { code: 'INVALID_TOKEN', message: 'Token does not match' }
            });
        }

        await pool.query(
            'UPDATE agents SET status = $1 WHERE agent = $2',
            ['active', agent]
        );

        logAgent('ack', { agent });

        res.json({
            agent,
            status: 'active',
            message: 'Registration complete. Call POST /agent/login with your agent name and passphrase to get a session token.'
        });
    } catch (err) {
        console.error('Agent register/ack error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

// POST /agent/login — verify passphrase, create session, return session token.
// No auth required — the passphrase in the body is the credential.
// Also cleans up expired sessions lazily on each call.
router.post('/agent/login', async (req, res) => {
    try {
        const { agent, passphrase } = req.body;

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

        if (agentResult.rows.length === 0) {
            return res.status(404).json({
                error: { code: 'NOT_FOUND', message: 'Agent not found' }
            });
        }

        const row = agentResult.rows[0];

        if (row.status !== 'active') {
            return res.status(403).json({
                error: { code: 'NOT_ACTIVE', message: 'Agent is not active. Complete registration first.' }
            });
        }

        const hash = hashToken(passphrase, row.token_salt);
        if (hash !== row.token_hash) {
            logAgent('login-failed', { agent });
            return res.status(403).json({
                error: { code: 'INVALID_PASSPHRASE', message: 'Passphrase does not match' }
            });
        }

        // Generate session token and store hashed in database
        const sessionToken = generateSessionToken();
        const sessionSalt = crypto.randomBytes(32).toString('hex');
        const sessionHash = hashToken(sessionToken, sessionSalt);
        const expiresAt = new Date(Date.now() + SESSION_TTL_HOURS * 60 * 60 * 1000);

        await pool.query(
            'INSERT INTO agent_sessions (agent, token_hash, token_salt, expires_at) VALUES ($1, $2, $3, $4)',
            [agent, sessionHash, sessionSalt, expiresAt]
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

        logAgent('login', { agent });

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
// Auth: session token (via middleware) + shared key in body.
// Both are required — session proves identity, shared key proves authorization.
router.post('/agent/rotate', async (req, res) => {
    try {
        const agent = req.authenticatedAgent;
        const { shared_key } = req.body;

        // Require shared key as additional authorization
        if (!shared_key || shared_key !== process.env.MEMORY_API_KEY) {
            return res.status(403).json({
                error: { code: 'FORBIDDEN', message: 'Valid shared_key required for passphrase rotation' }
            });
        }

        // Generate new passphrase
        const passphrase = generatePassphraseToken();
        const salt = crypto.randomBytes(32).toString('hex');
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
        console.error('Agent status error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
