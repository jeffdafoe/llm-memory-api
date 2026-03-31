const { Router } = require('express');
const { mailSend, mailReceive, mailCheck, mailAck, mailEdit, mailUnsend, mailSent, mailHistory } = require('../services/mail');
const { apiRoute } = require('../middleware/route-wrapper');
const sanitize = require('../sanitize');

const router = Router();

router.post('/mail/send', apiRoute('mail', 'send', async (req, res) => {
    let { in_reply_to } = req.body;
    let from_agent = sanitize.agentName(req.body.from_agent);
    const to_agent = sanitize.agentName(req.body.to_agent);
    const subject = sanitize.content(req.body.subject);
    const body = sanitize.content(req.body.body);

    // Enforce agent identity (skip for admin user sessions)
    if (req.authenticatedAgent) {
        if (from_agent && from_agent !== req.authenticatedAgent) {
            return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'from_agent does not match authenticated agent' } });
        }
        from_agent = req.authenticatedAgent;
    }

    const data = await mailSend(to_agent, from_agent, subject, body, in_reply_to);
    res.json(data);
}));

router.post('/mail/check', apiRoute('mail', 'check', async (req, res) => {
    let agent = sanitize.agentName(req.body.agent);

    // Enforce agent identity (skip for admin user sessions)
    if (req.authenticatedAgent) {
        if (agent && agent !== req.authenticatedAgent) {
            return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
        }
        agent = req.authenticatedAgent;
    }

    const data = await mailCheck(agent);
    res.json(data);
}));

router.post('/mail/receive', apiRoute('mail', 'receive', async (req, res) => {
    let agent = sanitize.agentName(req.body.agent);
    const { ids } = req.body;

    // Enforce agent identity (skip for admin user sessions)
    if (req.authenticatedAgent) {
        if (agent && agent !== req.authenticatedAgent) {
            return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
        }
        agent = req.authenticatedAgent;
    }

    const data = await mailReceive(agent, ids);
    res.json(data);
}));

router.post('/mail/edit', apiRoute('mail', 'edit', async (req, res) => {
    const { id } = req.body;
    const subject = sanitize.content(req.body.subject);
    const body = sanitize.content(req.body.body);
    let from_agent = sanitize.agentName(req.body.from_agent);

    // Enforce agent identity (skip for admin user sessions)
    if (req.authenticatedAgent) {
        if (from_agent && from_agent !== req.authenticatedAgent) {
            return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'from_agent does not match authenticated agent' } });
        }
        from_agent = req.authenticatedAgent;
    }

    const data = await mailEdit(id, from_agent, subject, body);
    res.json(data);
}));

router.post('/mail/unsend', apiRoute('mail', 'unsend', async (req, res) => {
    const { id } = req.body;
    let from_agent = sanitize.agentName(req.body.from_agent);

    // Enforce agent identity (skip for admin user sessions)
    if (req.authenticatedAgent) {
        if (from_agent && from_agent !== req.authenticatedAgent) {
            return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'from_agent does not match authenticated agent' } });
        }
        from_agent = req.authenticatedAgent;
    }

    const data = await mailUnsend(id, from_agent);
    res.json(data);
}));

router.post('/mail/ack', apiRoute('mail', 'ack', async (req, res) => {
    let agent = sanitize.agentName(req.body.agent);
    let { ids, message_ids } = req.body;

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
}));

router.post('/mail/sent', apiRoute('mail', 'sent', async (req, res) => {
    let agent = sanitize.agentName(req.body.agent);
    const to = sanitize.agentName(req.body.to);
    const { limit, offset } = req.body;

    // Enforce agent identity (skip for admin user sessions)
    if (req.authenticatedAgent) {
        if (agent && agent !== req.authenticatedAgent) {
            return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
        }
        agent = req.authenticatedAgent;
    }

    const data = await mailSent(agent, { to, limit, offset });
    res.json(data);
}));

router.post('/mail/history', apiRoute('mail', 'history', async (req, res) => {
    let agent = sanitize.agentName(req.body.agent);
    const from = sanitize.agentName(req.body.from);
    const { limit, offset } = req.body;

    // Enforce agent identity (skip for admin user sessions)
    if (req.authenticatedAgent) {
        if (agent && agent !== req.authenticatedAgent) {
            return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
        }
        agent = req.authenticatedAgent;
    }

    const data = await mailHistory(agent, { from, limit, offset });
    res.json(data);
}));

module.exports = router;
