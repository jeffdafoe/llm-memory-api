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

    // Tool-use plumbing (MEM-119) — all optional; absent means classic
    // text-only chat. tools_offered triggers the tool-use branch in
    // handleDirectChat. tool_calls is for senders that already have an
    // assistant tool-call to record (rare for direct routes — typically
    // handleDirectChat populates this on the reply row internally).
    // tool_call_id is set when this message is a tool result keyed to a
    // prior assistant tool_call.
    const toolCalls = req.body.tool_calls;
    const toolCallId = req.body.tool_call_id;
    const toolsOffered = req.body.tools_offered;
    if (toolCalls !== undefined && toolCalls !== null && !Array.isArray(toolCalls)) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'tool_calls must be an array' } });
    }
    if (toolsOffered !== undefined && toolsOffered !== null && !Array.isArray(toolsOffered)) {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'tools_offered must be an array' } });
    }
    if (toolCallId !== undefined && toolCallId !== null && typeof toolCallId !== 'string') {
        return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'tool_call_id must be a string' } });
    }

    const wait = req.body.wait === true;

    const result = await chatSend(from_agent, to_agents, discussionId, message, {
        toolCalls, toolCallId, toolsOffered,
    });

    // wait=true: hold the connection open until the VA reply lands inline.
    // Only meaningful when there's exactly one virtual-agent recipient;
    // chatSend exposes pendingReplyPromise in that case (and only that case).
    if (wait) {
        if (!result.pendingReplyPromise) {
            return res.status(400).json({
                error: {
                    code: 'NO_REPLY_PENDING',
                    message: 'wait=true requires exactly one virtual-agent recipient on a non-discussion chat',
                },
            });
        }
        try {
            const reply = await result.pendingReplyPromise;
            // pendingReplyPromise (from handleDirectChat) resolves with the
            // VA reply payload — text + tool_calls when tool-use was active,
            // or null when the legacy plain-text branch ran.
            return res.json({
                from_agent: result.from_agent,
                to_agents: result.to_agents,
                sent_at: result.sent_at,
                reply: reply || null,
            });
        } catch (replyErr) {
            // The VA reply path failed before sending its [Error] feedback
            // chat message. Surface the error directly so the wait-mode
            // caller can react instead of polling for a sentinel.
            return res.status(502).json({
                error: { code: 'REPLY_FAILED', message: replyErr.message || 'virtual agent reply failed' },
            });
        }
    }

    // Non-wait path: don't leak the internal promise to the JSON body.
    const { pendingReplyPromise: _unused, ...safeResult } = result;
    res.json(safeResult);
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
