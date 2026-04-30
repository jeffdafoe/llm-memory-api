const { Router } = require('express');
const { distillSimConversationDay } = require('../services/sim-conversation-distiller');
const { apiRoute } = require('../middleware/route-wrapper');
const sanitize = require('../sanitize');

const router = Router();

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

module.exports = router;
