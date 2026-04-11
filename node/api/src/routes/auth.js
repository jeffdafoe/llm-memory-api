// Auth verification endpoint — lets external services (e.g. ZBBS)
// check if a session token is valid without needing to duplicate
// the session validation logic.

const express = require('express');
const router = express.Router();
const { validateSessionToken } = require('../services/sessions');
const { SESSION_KIND } = require('../constants');

// POST /auth/verify
// Accepts a session token and returns whether it's valid and who it belongs to.
// No auth required on this endpoint — the token being verified IS the auth.
router.post('/auth/verify', async (req, res) => {
    const { token } = req.body;

    if (!token) {
        return res.status(400).json({ valid: false, error: 'Missing token' });
    }

    try {
        // Check web sessions (admin users who log in via browser)
        const webRow = await validateSessionToken(token, SESSION_KIND.WEB);
        if (webRow) {
            return res.json({
                valid: true,
                agent: webRow.name,
                actor_id: webRow.actor_id,
                realms: webRow.realms || [],
                session_kind: 'web',
            });
        }

        // Also check API sessions (agents that log in programmatically)
        const apiRow = await validateSessionToken(token, SESSION_KIND.API);
        if (apiRow) {
            return res.json({
                valid: true,
                agent: apiRow.name,
                actor_id: apiRow.actor_id,
                realms: apiRow.realms || [],
                session_kind: 'api',
            });
        }

        return res.json({ valid: false });
    } catch (err) {
        console.error('Auth verify error:', err.message);
        return res.status(500).json({ valid: false, error: 'Internal error' });
    }
});

module.exports = router;
