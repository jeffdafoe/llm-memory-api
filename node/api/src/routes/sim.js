const { Router } = require('express');
const pool = require('../db');
const { distillSimConversationDay } = require('../services/sim-conversation-distiller');
const { apiRoute } = require('../middleware/route-wrapper');
const { requirePerm } = require('../services/admin-permissions');
const { safeInt } = require('../util');
const sanitize = require('../sanitize');

const router = Router();

// UUID shape (any version) — scene_id is a UUID column, so a malformed value
// would hit PG as a cast error (500). We validate the format up front to answer
// a clean 400 instead.
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

// POST /v1/sim/conversation-day
//
// Receives a daily activity push from salem-engine for one sim NPC and
// builds a narrative conversations/YYYY-MM-DD-sim-day note in the
// agent's namespace. See services/sim-conversation-distiller.js for
// the full design rationale; this route is a thin authenticate-and-
// dispatch wrapper.
//
// Body: { agent: string, day: "YYYY-MM-DD", events: [{at, kind, payload}] }
//
// Idempotent — re-pushing the same (agent, day) overwrites the note.
// 400 for malformed payload, 404 for unknown agent, 400 for non-sim
// dream_mode.
router.post('/sim/conversation-day', apiRoute('sim', 'conversation_day', async (req, res) => {
    // Defensive body fallback — express.json gives us {} for empty bodies
    // but a missing Content-Type or unparseable body leaves req.body
    // undefined, which would throw inside sanitize.agentName before our
    // own validation in distillSimConversationDay can return a clean 400.
    const body = req.body || {};
    const agent = sanitize.agentName(body.agent);
    const day = body.day;
    const events = body.events;

    const result = await distillSimConversationDay(agent, day, events);
    res.json(result);
}));

// POST /v1/sim/raw-turns
//
// Operator-gated fetch of the full raw LLM turn(s) for sim debugging — the
// actual system_prompt the API composed, the user_message (perception) the
// engine sent, the provider's response, token counts, cost, and HTTP
// status/error — straight off virtual_agent_calls.
//
// This is the data backing the salem umbilical's /turns route: the salem engine
// never sees the system_prompt (it sends perception and gets back a reply; the
// API builds the system prompt per agent), so the only place the complete turn
// is logged is here. The umbilical proxies an operator's request to this
// endpoint, forwarding that operator's bearer token.
//
// Gated on plugins/administer — the same capability the umbilical's
// requireOperator checks — rather than the web-only admin-UI session. The
// operators (home/work) authenticate as AGENTS (api session / api key), which
// the standard /v1 auth middleware accepts and which carries req.actorId, so
// requirePerm works without requireUser.
//
// Body (all optional, ANDed together):
//   scene_id — exact scene UUID (one tick's cascade; the engine stamps it and
//              the API logs it on the call row — the precise NPC↔turn bridge)
//   agent    — the memory-api agent NAME (e.g. "zbbs-ezekiel-thorne", or the
//              shared "salem-vendor"/"salem-visitor"); 1:1 for a stateful NPC,
//              many-to-one for a shared VA
//   since    — ISO timestamp lower bound on created_at
//   status   — 'success' | 'error'
//   limit    — default 5, capped at 50 (each turn carries ~5k+14k chars of
//              prompt, so the default stays small; raise it deliberately)
//
// Returns { turns: [...] }, most-recent first.
//
// Indexing note: scene_id (idx_va_calls_scene) and agent (resolves to actor_id →
// idx_va_calls_actor_created) are both indexed, so the common debug queries are
// cheap. An UNfiltered call (no scene_id/agent) sorts the whole retention window
// by created_at — fine for an occasional operator call, but prefer a scene_id or
// agent filter when you can.
router.post('/sim/raw-turns', requirePerm('plugins', 'administer'), apiRoute('sim', 'raw_turns', async (req, res) => {
    const body = req.body || {};

    // Build the WHERE clause incrementally so every filter is optional and
    // parameterized (no string interpolation of user input into SQL).
    const conditions = [];
    const params = [];
    let idx = 1;

    if (body.scene_id !== undefined && body.scene_id !== null && body.scene_id !== '') {
        const sceneId = String(body.scene_id);
        if (!UUID_RE.test(sceneId)) {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'scene_id must be a UUID' } });
        }
        conditions.push(`c.scene_id = $${idx++}`);
        params.push(sceneId);
    }

    if (body.agent !== undefined && body.agent !== null && body.agent !== '') {
        if (typeof body.agent !== 'string') {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'agent must be a string' } });
        }
        // Cheap upper bound — agent names are short slugs; reject an oversized
        // string before it becomes a parameter the planner has to handle. (No
        // injection risk since it's bound, but this is a privileged debug route.)
        if (body.agent.length > 200) {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'agent is too long' } });
        }
        conditions.push(`ac.name = $${idx++}`);
        params.push(body.agent);
    }

    if (body.since !== undefined && body.since !== null && body.since !== '') {
        const since = new Date(body.since);
        if (Number.isNaN(since.getTime())) {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'since must be a valid timestamp' } });
        }
        conditions.push(`c.created_at >= $${idx++}`);
        params.push(since.toISOString());
    }

    if (body.status !== undefined && body.status !== null && body.status !== '') {
        if (body.status !== 'success' && body.status !== 'error') {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: "status must be 'success' or 'error'" } });
        }
        conditions.push(`c.status = $${idx++}`);
        params.push(body.status);
    }

    const limit = Math.min(Math.max(safeInt(body.limit) ?? 5, 1), 50);
    const where = conditions.length > 0 ? `WHERE ${conditions.join(' AND ')}` : '';

    const result = await pool.query(
        `SELECT c.id, ac.name AS agent, c.scene_id, c.context, c.context_id, c.provider, c.model,
                c.system_prompt, c.user_message, c.response,
                c.status, c.status_code, c.error_message,
                c.input_tokens, c.output_tokens, c.cache_read_tokens, c.cache_write_tokens,
                c.cost, c.duration_ms, c.created_at
         FROM virtual_agent_calls c
         JOIN actors ac ON ac.id = c.actor_id
         ${where}
         ORDER BY c.created_at DESC
         LIMIT $${idx}`,
        [...params, limit]
    );
    res.json({ turns: result.rows });
}));

module.exports = router;
