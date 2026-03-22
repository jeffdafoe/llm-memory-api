const express = require('express');
const crypto = require('crypto');
const router = express.Router();
const pool = require('../db');
const generatePassphrase = require('eff-diceware-passphrase');
const { generateSalt, hash: hashToken } = require('../services/hashing');

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
    const existing = await pool.query('SELECT id FROM actors WHERE name = $1', [agentName]);
    res.json({ available: existing.rows.length === 0 });
});

// POST /api/register — redeem invite code, create agent, return passphrase
router.post('/api/register', async (req, res) => {
    const { code, name } = req.body;
    if (!code || !name) {
        return res.status(400).json({ error: 'Invite code and agent name are required' });
    }

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

        // Check name availability
        const existing = await client.query('SELECT id FROM actors WHERE name = $1', [agentName]);
        if (existing.rows.length > 0) {
            await client.query('ROLLBACK');
            return res.status(409).json({ error: 'Agent name already taken' });
        }

        // Generate passphrase
        const words = generatePassphrase(3);
        const passphrase = words.join('-');
        const salt = generateSalt();
        const passphraseHash = hashToken(passphrase, salt);

        // Create actor
        await client.query(
            `INSERT INTO actors (name, token_hash, token_salt, status) VALUES ($1, $2, $3, 'active')`,
            [agentName, passphraseHash, salt]
        );

        // Create agent_configuration
        await client.query(
            `INSERT INTO agent_configuration (actor_id, provider, model)
             VALUES ((SELECT id FROM actors WHERE name = $1), NULL, NULL)`,
            [agentName]
        );

        // Mark invite code as used
        await client.query(
            `UPDATE invite_codes SET used_by = $1, used_at = NOW() WHERE id = $2`,
            [agentName, inv.id]
        );

        await client.query('COMMIT');

        res.json({
            ok: true,
            agent: agentName,
            passphrase,
            message: 'Account created. Save your passphrase — it will not be shown again.'
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
