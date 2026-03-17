const { Router } = require('express');
const { logError } = require('../services/logger');
const { mailSend, mailReceive, mailAck, mailEdit, mailUnsend, mailSent, mailHistory } = require('../services/mail');

const router = Router();

router.post('/mail/send', async (req, res) => {
    try {
        let { to_agent, from_agent, subject, body } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (from_agent && from_agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'from_agent does not match authenticated agent' } });
            }
            from_agent = req.authenticatedAgent;
        }

        const data = await mailSend(to_agent, from_agent, subject, body);
        res.json(data);
    } catch (err) {
        logError('mail', 'send', { agent: req.authenticatedAgent || req.body.from_agent, message: err.message, detail: err.stack });
        const status = err.statusCode || 500;
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : 'INTERNAL_ERROR', message: status >= 500 ? 'An internal error occurred' : err.message }
        });
    }
});

router.post('/mail/receive', async (req, res) => {
    try {
        let { agent } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (agent && agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
            }
            agent = req.authenticatedAgent;
        }

        const data = await mailReceive(agent);
        res.json(data);
    } catch (err) {
        logError('mail', 'receive', { agent: req.authenticatedAgent || req.body.agent, message: err.message, detail: err.stack });
        const status = err.statusCode || 500;
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : 'INTERNAL_ERROR', message: status >= 500 ? 'An internal error occurred' : err.message }
        });
    }
});

router.post('/mail/edit', async (req, res) => {
    try {
        let { id, from_agent, subject, body } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (from_agent && from_agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'from_agent does not match authenticated agent' } });
            }
            from_agent = req.authenticatedAgent;
        }

        const data = await mailEdit(id, from_agent, subject, body);
        res.json(data);
    } catch (err) {
        logError('mail', 'edit', { agent: req.authenticatedAgent || req.body.from_agent, message: err.message, detail: err.stack });
        const status = err.statusCode || 500;
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : 'INTERNAL_ERROR', message: status >= 500 ? 'An internal error occurred' : err.message }
        });
    }
});

router.post('/mail/unsend', async (req, res) => {
    try {
        let { id, from_agent } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (from_agent && from_agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'from_agent does not match authenticated agent' } });
            }
            from_agent = req.authenticatedAgent;
        }

        const data = await mailUnsend(id, from_agent);
        res.json(data);
    } catch (err) {
        logError('mail', 'unsend', { agent: req.authenticatedAgent || req.body.from_agent, message: err.message, detail: err.stack });
        const status = err.statusCode || 500;
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : 'INTERNAL_ERROR', message: status >= 500 ? 'An internal error occurred' : err.message }
        });
    }
});

router.post('/mail/ack', async (req, res) => {
    try {
        let { agent, ids, message_ids } = req.body;

        // Accept both 'ids' (canonical) and 'message_ids' (legacy) — remove message_ids after 2026-04-15
        ids = ids || message_ids;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (agent && agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
            }
            agent = req.authenticatedAgent;
        }

        const data = await mailAck(agent, ids);
        res.json(data);
    } catch (err) {
        logError('mail', 'ack', { agent: req.authenticatedAgent || req.body.agent, message: err.message, detail: err.stack });
        const status = err.statusCode || 500;
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : 'INTERNAL_ERROR', message: status >= 500 ? 'An internal error occurred' : err.message }
        });
    }
});

router.post('/mail/sent', async (req, res) => {
    try {
        let { agent, to, limit, offset } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (agent && agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
            }
            agent = req.authenticatedAgent;
        }

        const data = await mailSent(agent, { to, limit, offset });
        res.json(data);
    } catch (err) {
        logError('mail', 'sent', { agent: req.authenticatedAgent || req.body.agent, message: err.message, detail: err.stack });
        const status = err.statusCode || 500;
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : 'INTERNAL_ERROR', message: status >= 500 ? 'An internal error occurred' : err.message }
        });
    }
});

router.post('/mail/history', async (req, res) => {
    try {
        let { agent, from, limit, offset } = req.body;

        // Enforce agent identity (skip for admin user sessions)
        if (req.authenticatedAgent) {
            if (agent && agent !== req.authenticatedAgent) {
                return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
            }
            agent = req.authenticatedAgent;
        }

        const data = await mailHistory(agent, { from, limit, offset });
        res.json(data);
    } catch (err) {
        logError('mail', 'history', { agent: req.authenticatedAgent || req.body.agent, message: err.message, detail: err.stack });
        const status = err.statusCode || 500;
        res.status(status).json({
            error: { code: status === 400 ? 'BAD_REQUEST' : 'INTERNAL_ERROR', message: status >= 500 ? 'An internal error occurred' : err.message }
        });
    }
});

module.exports = router;
