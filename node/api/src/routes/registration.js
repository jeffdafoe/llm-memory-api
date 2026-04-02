const express = require('express');
const crypto = require('crypto');
const router = express.Router();
const pool = require('../db');
const config = require('../services/config');
const generatePassphrase = require('eff-diceware-passphrase');
const { generateSalt, hash: hashToken, generateKey } = require('../services/hashing');
const { mailSend } = require('../services/mail');
const { checkNameAvailability, moderateActorName } = require('../services/actors');

// Parse YAML-style frontmatter from template content (same logic as admin.js)
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

// GET /api/registration-mode — check if open registration is enabled
router.get('/api/registration-mode', (req, res) => {
    const open = config.get('open_registration') === 'true';
    res.json({ open });
});

// POST /api/check-name — check if an agent name is available
router.post('/api/check-name', async (req, res) => {
    const { name } = req.body;
    if (!name || !name.trim()) {
        return res.status(400).json({ error: 'Name is required' });
    }
    const agentName = name.trim().toLowerCase();
    if (!/^[a-z][a-z0-9_-]{1,30}$/.test(agentName)) {
        return res.json({ available: false, reason: 'Name must start with a letter, 2-31 chars, only lowercase letters, numbers, hyphens, underscores.' });
    }
    const result = await checkNameAvailability(agentName);
    res.json(result);
});

// POST /api/register — redeem invite code, create agent, return passphrase
router.post('/api/register', async (req, res) => {
    const { code, name } = req.body;

    if (!code || !name) {
        return res.status(400).json({ error: 'Invite code and agent name are required' });
    }

    const { password, dream_mode } = req.body;
    if (!password || password.length < 8) {
        return res.status(400).json({ error: 'Password must be at least 8 characters' });
    }
    const validDreamModes = ['none', 'companion', 'technical'];
    const dreamMode = validDreamModes.includes(dream_mode) ? dream_mode : 'none';

    const agentName = name.trim().toLowerCase();
    if (!/^[a-z][a-z0-9_-]{1,30}$/.test(agentName)) {
        return res.status(400).json({ error: 'Name must start with a letter, 2-31 chars, only lowercase letters, numbers, hyphens, underscores.' });
    }

    const client = await pool.connect();
    try {
        await client.query('BEGIN');

        // Validate invite code
        const invite = await client.query(
            `SELECT id, used_by, expires_at FROM invite_codes WHERE code = $1 FOR UPDATE`,
            [code.trim()]
        );
        if (invite.rows.length === 0) {
            await client.query('ROLLBACK');
            return res.status(400).json({ error: 'Invalid invite code' });
        }
        const inv = invite.rows[0];
        if (inv.used_by) {
            await client.query('ROLLBACK');
            return res.status(400).json({ error: 'This invite code has already been used' });
        }
        if (inv.expires_at && new Date(inv.expires_at) < new Date()) {
            await client.query('ROLLBACK');
            return res.status(400).json({ error: 'This invite code has expired' });
        }

        // Check name availability (existing actors, existing namespaces)
        const availability = await checkNameAvailability(agentName);
        if (!availability.available) {
            await client.query('ROLLBACK');
            return res.status(409).json({ error: availability.reason });
        }

        // Virtual agent moderation check (skips gracefully if VA not configured)
        const moderation = await moderateActorName(agentName);
        if (!moderation.approved) {
            await client.query('ROLLBACK');
            return res.status(400).json({ error: moderation.reason });
        }

        // Generate passphrase
        const words = generatePassphrase(3);
        const passphrase = words.join('-');
        const salt = generateSalt();
        const passphraseHash = hashToken(passphrase, salt);

        // Hash the dashboard password
        const passwordSalt = generateSalt();
        const passwordHash = hashToken(password, passwordSalt);

        // Create actor (created_by is set to self after insert)
        await client.query(
            `INSERT INTO actors (name, token_hash, token_salt, password_hash, password_salt, status) VALUES ($1, $2, $3, $4, $5, 'active')`,
            [agentName, passphraseHash, salt, passwordHash, passwordSalt]
        );
        // Set created_by to self so the agent owns itself
        await client.query(
            `UPDATE actors SET created_by = id WHERE name = $1`,
            [agentName]
        );

        // Create agent_configuration
        await client.query(
            `INSERT INTO agent_configuration (actor_id, provider, model, dream_mode)
             VALUES ((SELECT id FROM actors WHERE name = $1), NULL, NULL, $2)`,
            [agentName, dreamMode]
        );

        // Mark invite code as used
        await client.query(
            `UPDATE invite_codes SET used_by = $1, used_at = NOW() WHERE id = $2`,
            [agentName, inv.id]
        );

        // Get the new actor's ID for permission grants
        const actorResult = await client.query('SELECT id FROM actors WHERE name = $1', [agentName]);
        const actorId = actorResult.rows[0].id;

        // Grant all MCP permissions (full tool access)
        await client.query(
            `INSERT INTO agent_permissions (actor_id, permission_id)
             SELECT $1, id FROM permissions`,
            [actorId]
        );

        // Own-namespace access is implicit (enforced in namespace-permissions.js hasAccess).
        // No namespace_permissions row needed.

        // Grant admin UI access (dashboard, agents, communications)
        await client.query(
            `INSERT INTO admin_permissions (actor_id, resource, action) VALUES
             ($1, 'dashboard', 'read'),
             ($1, 'agents', 'read'),
             ($1, 'agents', 'write'),
             ($1, 'comms', 'read')`,
            [actorId]
        );

        // Generate API key for MCP/OAuth authentication
        const apiKey = generateKey();
        const apiKeySalt = generateSalt();
        const apiKeyHash = hashToken(apiKey, apiKeySalt);
        await client.query(
            `INSERT INTO agent_api_keys (actor_id, key_hash, key_salt, label) VALUES ($1, $2, $3, 'default')`,
            [actorId, apiKeyHash, apiKeySalt]
        );

        await client.query('COMMIT');

        // Apply default welcome template (same as admin-created agents)
        // Runs after commit so the agent exists even if template fails
        try {
            const tplResult = await pool.query(
                "SELECT content FROM templates WHERE kind = 'welcome' ORDER BY id LIMIT 1"
            );
            if (tplResult.rows.length > 0) {
                const rawContent = tplResult.rows[0].content;
                const { frontmatter, body: tplBody } = parseTemplateFrontmatter(rawContent);
                const mailBody = tplBody.replace(/\{agent\}/g, agentName);

                // Copy template body to startup_instructions (persistent)
                await pool.query(
                    'UPDATE agent_configuration SET startup_instructions = $1 WHERE actor_id = (SELECT id FROM actors WHERE name = $2)',
                    [mailBody, agentName]
                );

                // Also send as welcome mail
                const mailSubject = (frontmatter.subject || 'Welcome, {agent}').replace(/\{agent\}/g, agentName);
                await mailSend(agentName, 'system', mailSubject, mailBody);
            }
        } catch (tplErr) {
            // Don't fail registration if template application fails
            console.error('Registration welcome template error:', tplErr.message);
        }

        res.json({
            ok: true,
            agent: agentName,
            passphrase,
            api_key: apiKey,
            message: 'Account created. Save your passphrase and API key — they will not be shown again.'
        });
    } catch (err) {
        await client.query('ROLLBACK');
        console.error('Registration error:', err.message);
        res.status(500).json({ error: 'Something went wrong' });
    } finally {
        client.release();
    }
});

module.exports = router;
