const { Router } = require('express');
const { chatSend, chatReceive, chatAck, chatStatus, resolveDiscussionId } = require('../services/chat');
const { apiRoute } = require('../middleware/route-wrapper');
const sanitize = require('../sanitize');

const router = Router();

router.post('/chat/send', apiRoute('chat', 'send', async (req, res) => {
    let message = sanitize.content(req.body.message);
    let from_agent = sanitize.agentName(req.body.from_agent);
    let to_agents = Array.isArray(req.body.to_agents)
        ? req.body.to_agents.map(a => a === '*' ? a : sanitize.agentName(a))
        : req.body.to_agents;

    // Enforce agent identity (skip for admin user sessions)
    if (req.authenticatedAgent) {
        if (from_agent && from_agent !== req.authenticatedAgent) {
            return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'from_agent does not match authenticated agent' } });
        }
        from_agent = req.authenticatedAgent;
    }

    // Accept discussion_id directly, or extract from legacy channel string
    const discussionId = resolveDiscussionId(req.body.discussion_id, req.body.channel);

    const result = await chatSend(from_agent, to_agents, discussionId, message);
    res.json(result);
}));

router.post('/chat/receive', apiRoute('chat', 'receive', async (req, res) => {
    let agent = sanitize.agentName(req.body.agent);
    let from_agent = sanitize.agentName(req.body.from_agent);
    const { after_id } = req.body;

    // Enforce agent identity (skip for admin user sessions)
    if (req.authenticatedAgent) {
        if (agent && agent !== req.authenticatedAgent) {
            return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
        }
        agent = req.authenticatedAgent;
    }

    if (after_id !== undefined && (!Number.isInteger(after_id) || after_id < 0)) {
        return res.status(400).json({
            error: { code: 'BAD_REQUEST', message: 'after_id must be a non-negative integer' }
        });
    }

    // Accept discussion_id directly, or extract from legacy channel string
    const discussionId = resolveDiscussionId(req.body.discussion_id, req.body.channel);

    const result = await chatReceive(agent, discussionId, after_id, from_agent);
    res.json(result);
}));

router.post('/chat/ack', apiRoute('chat', 'ack', async (req, res) => {
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

    const result = await chatAck(agent, ids);
    res.json(result);
}));

router.post('/chat/status', apiRoute('chat', 'status', async (req, res) => {
    let agent = sanitize.agentName(req.body.agent);

    // Enforce agent identity (skip for admin user sessions)
    if (req.authenticatedAgent) {
        if (agent && agent !== req.authenticatedAgent) {
            return res.status(403).json({ error: { code: 'IDENTITY_MISMATCH', message: 'agent does not match authenticated agent' } });
        }
        agent = req.authenticatedAgent;
    }

    // Accept discussion_id directly, or extract from legacy channel string
    const discussionId = resolveDiscussionId(req.body.discussion_id, req.body.channel);

    const result = await chatStatus(agent, discussionId);
    res.json(result);
}));

module.exports = router;
