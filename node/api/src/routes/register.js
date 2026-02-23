const { Router } = require('express');
const crypto = require('crypto');
const fs = require('fs');
const path = require('path');
const generatePassphrase = require('eff-diceware-passphrase');
const pool = require('../db');
const { log } = require('../services/logger');

const router = Router();

function logRegister(action, details) {
    log('register', action, details);
}

function hashToken(plaintext, salt) {
    return crypto.pbkdf2Sync(plaintext, salt, 100000, 64, 'sha512').toString('hex');
}

function generateToken() {
    const words = generatePassphrase(3);
    return words.join('-');
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

// POST /register — create agent, generate token, return plaintext
router.post('/register', async (req, res) => {
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
            const token = generateToken();
            const salt = crypto.randomBytes(32).toString('hex');
            const hash = hashToken(token, salt);

            await pool.query(
                'UPDATE agents SET token_hash = $1, token_salt = $2, status = $3 WHERE agent = $4',
                [hash, salt, 'pending', agent]
            );

            logRegister('re-register', { agent });

            return res.json({
                agent,
                token,
                status: 'pending',
                message: 'Token regenerated. Call POST /register/ack with your agent name and token to activate.',
                onboarding: buildOnboarding(agent)
            });
        }

        // New agent
        const token = generateToken();
        const salt = crypto.randomBytes(32).toString('hex');
        const hash = hashToken(token, salt);

        await pool.query(
            'INSERT INTO agents (agent, token_hash, token_salt, status) VALUES ($1, $2, $3, $4)',
            [agent, hash, salt, 'pending']
        );

        logRegister('register', { agent });

        res.json({
            agent,
            token,
            status: 'pending',
            message: 'Save this token — it will not be shown again. Call POST /register/ack with your agent name and token to activate.',
            onboarding: buildOnboarding(agent)
        });
    } catch (err) {
        console.error('Register error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

// POST /register/ack — validate token, activate agent
router.post('/register/ack', async (req, res) => {
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

        logRegister('ack', { agent });

        res.json({
            agent,
            status: 'active',
            message: 'Registration complete. Use your token as Authorization: Bearer <token> for all API calls.'
        });
    } catch (err) {
        console.error('Register ack error:', err.message);
        res.status(500).json({
            error: { code: 'INTERNAL_ERROR', message: err.message }
        });
    }
});

module.exports = router;
