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
//   conversation — the salem huddle id (hud-<hex>) the engine threads onto
//              conversation_id via /v1/chat/send (ZBBS-HOME-397). The one-call
//              "every turn in this huddle's conversation" filter (ZBBS-WORK-431),
//              stable across the huddle's ticks and participants. TEXT, not a UUID
//              like scene_id, so no format validation — just a length bound.
//   since    — ISO timestamp lower bound on created_at
//   until    — EXCLUSIVE upper bound on created_at (strictly earlier-than).
//              The route returns the newest rows first with no offset
//              pagination, so without this an older episode buried behind 50+
//              newer turns is unreachable. Exclusive so it works as a cursor:
//              pass the oldest row's created_at verbatim to fetch the next
//              page back without repeating the boundary row.
//   status   — 'success' | 'error'
//   limit    — when a bounding filter (scene_id / conversation / agent) is set,
//              the result is bounded and indexed, so it returns the FULL set by
//              default (capped at 500) — the point of those filters is "give me
//              this conversation," not a 5-row tail of it. With NO bounding
//              filter the result is a tail of the whole retention window and
//              each turn carries ~5k+14k chars of prompt, so it stays small:
//              default 5, capped at 50 — raise it deliberately, or use `until`
//              as a walk-back cursor.
//
// Returns { turns: [...], returned, has_more }, most-recent first. has_more is
// true when more rows matched than were returned (a partial tail), so truncation
// is never silent. It's computed by over-fetching one row rather than a COUNT,
// so the unfiltered tail still stops at the index instead of scanning the whole
// retention window just to report a total.
//
// Indexing note: scene_id (idx_va_calls_scene), agent (resolves to actor_id →
// idx_va_calls_actor_created), and conversation (idx_va_calls_conversation, the
// partial index from MEM-133) are all indexed, so the common debug queries are
// cheap. An UNfiltered call (no scene_id/agent/conversation) sorts the whole
// retention window by created_at — fine for an occasional operator call, but
// prefer a filter when you can.
router.post('/sim/raw-turns', requirePerm('plugins', 'administer'), apiRoute('sim', 'raw_turns', async (req, res) => {
    const body = req.body || {};

    // Build the WHERE clause incrementally so every filter is optional and
    // parameterized (no string interpolation of user input into SQL).
    const conditions = [];
    const params = [];
    let idx = 1;
    // A scene_id / conversation / agent filter bounds the result to a single
    // scene, conversation, or NPC — each backed by an index — so a bounded query
    // returns its complete set by default (see the limit handling below). since /
    // until / status narrow but don't bound, so they don't flip this.
    let hasBoundingFilter = false;

    if (body.scene_id !== undefined && body.scene_id !== null && body.scene_id !== '') {
        const sceneId = String(body.scene_id);
        if (!UUID_RE.test(sceneId)) {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'scene_id must be a UUID' } });
        }
        conditions.push(`c.scene_id = $${idx++}`);
        params.push(sceneId);
        hasBoundingFilter = true;
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
        hasBoundingFilter = true;
    }

    if (body.conversation !== undefined && body.conversation !== null && body.conversation !== '') {
        if (typeof body.conversation !== 'string') {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'conversation must be a string' } });
        }
        // conversation_id is the salem huddle id (hud-<hex>) — a TEXT column (not a
        // UUID like scene_id), so no format cast is needed. Cap the length on this
        // privileged debug route the same way `agent` is bounded; the value is a
        // bound parameter, so there's no injection risk either way.
        if (body.conversation.length > 200) {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'conversation is too long' } });
        }
        conditions.push(`c.conversation_id = $${idx++}`);
        params.push(body.conversation);
        hasBoundingFilter = true;
    }

    if (body.since !== undefined && body.since !== null && body.since !== '') {
        const since = new Date(body.since);
        if (Number.isNaN(since.getTime())) {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'since must be a valid timestamp' } });
        }
        conditions.push(`c.created_at >= $${idx++}`);
        params.push(since.toISOString());
    }

    if (body.until !== undefined && body.until !== null && body.until !== '') {
        const until = new Date(body.until);
        if (Number.isNaN(until.getTime())) {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: 'until must be a valid timestamp' } });
        }
        conditions.push(`c.created_at < $${idx++}`);
        params.push(until.toISOString());
    }

    if (body.status !== undefined && body.status !== null && body.status !== '') {
        if (body.status !== 'success' && body.status !== 'error') {
            return res.status(400).json({ error: { code: 'BAD_REQUEST', message: "status must be 'success' or 'error'" } });
        }
        conditions.push(`c.status = $${idx++}`);
        params.push(body.status);
    }

    // A bounded query (scene_id / conversation / agent) returns its complete set
    // by default — the filter already scopes it to one conversation/scene/NPC, so
    // a small default would silently truncate the very thing the caller asked for.
    // An unbounded query is a tail of the whole retention window where each row
    // carries a multi-KB prompt, so it stays small by default and must be raised
    // deliberately.
    const unboundedDefaultLimit = 5;
    const unboundedMaxLimit = 50;
    const boundedMaxLimit = 500;
    const defaultLimit = hasBoundingFilter ? boundedMaxLimit : unboundedDefaultLimit;
    const maxLimit = hasBoundingFilter ? boundedMaxLimit : unboundedMaxLimit;
    const limit = Math.min(Math.max(safeInt(body.limit) ?? defaultLimit, 1), maxLimit);

    const where = conditions.length > 0 ? `WHERE ${conditions.join(' AND ')}` : '';

    // Over-fetch one row past the limit to detect truncation without a COUNT. A
    // COUNT(*) OVER() would force the unfiltered tail to walk the whole retention
    // window instead of stopping at the index after `limit` rows; the one extra
    // row is enough to set has_more, and it's trimmed before returning.
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
        [...params, limit + 1]
    );

    const hasMore = result.rows.length > limit;
    const turns = hasMore ? result.rows.slice(0, limit) : result.rows;
    res.json({ turns, returned: turns.length, has_more: hasMore });
}));

module.exports = router;
