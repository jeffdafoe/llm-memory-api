// Sim-conversation distiller — daily per-NPC narrative builder.
//
// Receives a typed event payload from salem-engine (one push per sim NPC
// per just-completed in-sim day), joins it with this side's own
// chat_message_texts (agent speech, multi-party scene chatter), and
// produces a single conversations/YYYY-MM-DD-sim-day note in the agent's
// namespace formatted as `[Day HH:MM Speaker] text` lines.
//
// The replacement lives next to the dream pipeline rather than the per-
// /chat/send transcript writer because:
//
//   - The per-call writer (logTranscript) saves the raw chat-completion
//     payload (system prompt + JSON-stringified user messages + response).
//     For sim NPCs that's accumulating per-tick hourly — John Ellis's
//     most recent ran 77K+ chars before this fix.
//
//   - The dream prefilter (SIGNAL_PATTERNS in dream.js) is tuned for
//     human-AI conversation cues ("remember", "worried", "thank you").
//     Sim perception text ("You feel: peckish, parched, tired") doesn't
//     hit those signals, so the prefilter throws most of it away. Even
//     when something matches, the ±5 context lines are JSON fragments.
//
// The logTranscript path now skips dream_mode='sim' agents entirely; this
// distiller is the only thing writing conversations/* notes for them.
// chat_message_texts is still written for audit/replay.
//
// Engine event shape — caller sends an array of:
//   { at: ISO timestamp, kind: action_type, payload: object }
// Kinds correspond to agent_action_log.action_type values: 'move_to',
// 'chore', 'pay', 'object_refresh', etc. Unknown kinds get a generic
// narration so a new engine action_type doesn't silently drop frames.

const pool = require('./db');
const { saveNote } = require('./documents');
const { log, logError } = require('./logger');

function logSim(event, payload) {
    log('sim-distill', { event, ...payload });
}

// Format a YYYY-MM-DD day string into a UTC window [00:00, next 00:00).
// Engine pushes the just-completed day; this is the boundary the
// chat_message_texts query uses for sent_at.
//
// The round-trip check catches syntactically valid but non-existent
// dates like 2026-02-31 — JS silently normalizes that to March 3 and
// would happily distill the wrong day under the wrong slug. Reject
// the input cleanly instead.
function dayWindow(dayStr) {
    const start = new Date(dayStr + 'T00:00:00.000Z');
    if (isNaN(start.getTime()) || start.toISOString().slice(0, 10) !== dayStr) {
        throw Object.assign(new Error('invalid day: ' + dayStr), { statusCode: 400 });
    }
    const end = new Date(start.getTime() + 24 * 60 * 60 * 1000);
    return { start, end };
}

// Render a UTC timestamp as "Weekday HH:MM" matching the work-agent
// distillation shape. Sim narrative reads more naturally with the day
// label inline; tz is whatever the engine pushed (no conversion here —
// engine controls the wall-clock interpretation).
const WEEKDAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
function formatTimestamp(iso) {
    const d = new Date(iso);
    if (isNaN(d.getTime())) {
        return '??:??';
    }
    const day = WEEKDAYS[d.getUTCDay()];
    const hh = String(d.getUTCHours()).padStart(2, '0');
    const mm = String(d.getUTCMinutes()).padStart(2, '0');
    return day + ' ' + hh + ':' + mm;
}

// Sanitize a label (speaker name, location, recipient) so it can't
// inject brackets, newlines, or quotes that break the [Day HH:MM
// Speaker] line shape. Display names and engine-pushed payload values
// are user/DB-controlled — defensive normalization here keeps the
// transcript parseable even with surprising input.
function sanitizeLabel(s) {
    return String(s || '')
        .replace(/[\r\n\[\]"]/g, ' ')
        .replace(/\s+/g, ' ')
        .trim();
}

// Sanitize quoted speech text: collapse whitespace, escape backslash
// and double-quote so the closing `"` of the speech wrapper isn't
// pre-empted by content. Keeps each speech line on a single
// transcript line, even when the original message contained newlines
// (sim NPCs occasionally include narration mid-quote).
function sanitizeSpeech(s) {
    return String(s || '')
        .replace(/\\/g, '\\\\')
        .replace(/"/g, '\\"')
        .replace(/\s+/g, ' ')
        .trim();
}

// Map an engine event to a narration line. Returns the (action) text
// rendered in parens, or null when the event carries no narrative
// signal (look_around, done with no state change). Keeping this in
// one place means a new action_type lands an entry once and shows up
// everywhere consistently.
function narrateEvent(event, actorName) {
    const p = event.payload || {};
    switch (event.kind) {
        case 'move_to': {
            const dest = sanitizeLabel(p.destination || p.structure_name || 'somewhere');
            return '(walked to ' + dest + ')';
        }
        case 'chore': {
            const type = sanitizeLabel(p.type || 'an errand');
            // Chore types map to specific verbs where the engine has
            // semantic context for one. Generic fallback for unknown.
            const phrasing = {
                well: 'fetched water from the well',
                outhouse: 'visited the outhouse',
                shop: 'ran an errand at the shop',
            };
            return '(' + (phrasing[type] || ('ran an errand: ' + type)) + ')';
        }
        case 'pay': {
            const recipient = sanitizeLabel(p.recipient || p.recipient_name || 'someone');
            // Coerce to a non-negative integer before rendering. A
            // string-shaped amount in the engine payload (rare, but
            // possible if a future caller stringifies) could otherwise
            // inject brackets/newlines into the transcript shape.
            const rawAmount = Number(p.amount);
            const amount = Number.isFinite(rawAmount) && rawAmount > 0 ? Math.floor(rawAmount) : 0;
            const forText = sanitizeLabel(p.for || p.for_text || '');
            const coins = amount === 1 ? '1 coin' : amount + ' coins';
            const reason = forText ? ' for ' + forText : '';
            return '(paid ' + recipient + ' ' + coins + reason + ')';
        }
        case 'object_refresh': {
            const refreshes = Array.isArray(p.refreshes) ? p.refreshes : [];
            const objectName = sanitizeLabel(p.object_name || 'something');
            // Pick a verb per attribute; multi-attribute objects (a
            // shaded oak: tiredness + hunger) get joined.
            const verbs = refreshes.map((r) => {
                switch (r.attribute) {
                    case 'thirst': return 'drank';
                    case 'hunger': return 'ate';
                    case 'tiredness': return 'rested';
                    default: return 'drew on ' + sanitizeLabel(r.attribute);
                }
            }).filter(Boolean);
            if (verbs.length === 0) {
                return '(arrived at ' + objectName + ')';
            }
            const verbText = verbs.length === 1 ? verbs[0] : verbs.slice(0, -1).join(', ') + ' and ' + verbs[verbs.length - 1];
            return '(' + verbText + ' at ' + objectName + ')';
        }
        case 'look_around':
        case 'done':
            // Pure-perception / pass-the-hour actions carry no narrative
            // content on their own. Drop them — accumulating "Josiah
            // looked around" lines is the noise that broke the old
            // raw-payload format.
            return null;
        default:
            // Unknown action_type — surface as generic narration so the
            // line isn't silently lost. Better than dropping; signals
            // that a new engine action_type needs a real mapping here.
            return '(' + actorName + ' ' + sanitizeLabel(event.kind) + ')';
    }
}

// Pull all speech the agent was party to in the day window: their own
// speech (1-on-1 outbound), speech directed at them (1-on-1 inbound),
// and every message in any scene or discussion the agent had day-window
// activity in. Returns rows sorted by sent_at.
//
// Two-stage: first discover the scene/discussion IDs the agent had a
// message touching during this window (sender or recipient), then
// pull every message in those groupings plus standalone 1-on-1. The
// day-window scope on the discovery query is important — pulling all
// historical participants would over-include speech from rooms the
// agent had already left or that re-opened later.
//
// EXISTS used instead of LEFT JOIN to avoid row duplication when one
// chat_message_texts row has multiple chat_messages delivery rows
// (multicast — addressed to several recipients).
async function fetchSpeech(actorId, start, end) {
    const sceneIdsResult = await pool.query(
        `SELECT DISTINCT cmt.scene_id
         FROM chat_message_texts cmt
         WHERE cmt.sent_at >= $2 AND cmt.sent_at < $3
           AND cmt.scene_id IS NOT NULL
           AND (
               cmt.from_actor_id = $1
               OR EXISTS (
                   SELECT 1 FROM chat_messages cm
                   WHERE cm.message_text_id = cmt.id AND cm.to_actor_id = $1
               )
           )`,
        [actorId, start, end]
    );
    const sceneIds = sceneIdsResult.rows.map((r) => r.scene_id);

    const discIdsResult = await pool.query(
        `SELECT DISTINCT cmt.discussion_id
         FROM chat_message_texts cmt
         WHERE cmt.sent_at >= $2 AND cmt.sent_at < $3
           AND cmt.discussion_id IS NOT NULL
           AND (
               cmt.from_actor_id = $1
               OR EXISTS (
                   SELECT 1 FROM chat_messages cm
                   WHERE cm.message_text_id = cmt.id AND cm.to_actor_id = $1
               )
           )`,
        [actorId, start, end]
    );
    const discussionIds = discIdsResult.rows.map((r) => r.discussion_id);

    // Now pull every message in those groupings plus 1-on-1 to/from
    // the agent. EXISTS for the recipient leg keeps the row count
    // honest when a message has multiple delivery rows.
    const speechResult = await pool.query(
        `SELECT cmt.id, cmt.message, cmt.sent_at, cmt.scene_id, cmt.discussion_id,
                cmt.from_actor_id, ac.name AS from_actor_name, ac.display_name AS from_display_name
         FROM chat_message_texts cmt
         JOIN actors ac ON ac.id = cmt.from_actor_id
         WHERE cmt.sent_at >= $2 AND cmt.sent_at < $3
           AND (cmt.is_error IS NOT TRUE)
           AND (
               (cmt.scene_id = ANY($4::uuid[]))
               OR (cmt.discussion_id = ANY($5::int[]))
               OR (
                   cmt.scene_id IS NULL
                   AND cmt.discussion_id IS NULL
                   AND (
                       cmt.from_actor_id = $1
                       OR EXISTS (
                           SELECT 1 FROM chat_messages cm
                           WHERE cm.message_text_id = cmt.id AND cm.to_actor_id = $1
                       )
                   )
               )
           )
         ORDER BY cmt.sent_at ASC, cmt.id ASC`,
        [actorId, start, end, sceneIds, discussionIds]
    );
    return speechResult.rows;
}

// Resolve the agent name to (actor_id, display_name, dream_mode). The
// distiller is sim-only — return null if dream_mode != 'sim' so the
// caller can reject without producing an empty note.
async function resolveSimAgent(agentName) {
    const r = await pool.query(
        `SELECT ac.id, ac.name, ac.display_name, agc.dream_mode
         FROM actors ac
         JOIN agent_configuration agc ON agc.actor_id = ac.id
         WHERE ac.name = $1`,
        [agentName]
    );
    if (r.rows.length === 0) {
        return null;
    }
    const row = r.rows[0];
    if (row.dream_mode !== 'sim') {
        return { ...row, _wrongMode: true };
    }
    return row;
}

// Main entry — receives an engine push, builds and saves the note.
// Idempotent: re-running the same (agent, day, events) overwrites the
// existing note. Returns a small summary for the caller.
async function distillSimConversationDay(agentName, dayStr, events) {
    if (!agentName || typeof agentName !== 'string') {
        throw Object.assign(new Error('agent required'), { statusCode: 400 });
    }
    if (!dayStr || typeof dayStr !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(dayStr)) {
        throw Object.assign(new Error('day required (YYYY-MM-DD)'), { statusCode: 400 });
    }
    if (!Array.isArray(events)) {
        throw Object.assign(new Error('events must be an array'), { statusCode: 400 });
    }

    const agent = await resolveSimAgent(agentName);
    if (!agent) {
        throw Object.assign(new Error('unknown agent: ' + agentName), { statusCode: 404 });
    }
    if (agent._wrongMode) {
        throw Object.assign(
            new Error('agent ' + agentName + ' is not configured for sim dream_mode'),
            { statusCode: 400 }
        );
    }

    const { start, end } = dayWindow(dayStr);
    const actorName = sanitizeLabel(agent.display_name || agent.name);

    // Build the per-event narration lines first, then merge with speech
    // by timestamp. Skip events that fall outside the day window — the
    // engine should only push events for the requested day, but defend
    // against off-by-one.
    const narrationLines = [];
    for (const event of events) {
        if (!event || typeof event.at !== 'string' || typeof event.kind !== 'string') {
            continue;
        }
        const at = new Date(event.at);
        if (isNaN(at.getTime()) || at < start || at >= end) {
            continue;
        }
        const text = narrateEvent(event, actorName);
        if (!text) {
            continue;
        }
        narrationLines.push({
            at,
            line: '[' + formatTimestamp(event.at) + ' ' + actorName + '] ' + text,
        });
    }

    // Pull speech. fetchSpeech handles scenes + discussions + 1-on-1.
    const speechRows = await fetchSpeech(agent.id, start, end);
    const speechLines = speechRows.map((row) => {
        // Use display_name when set; fall back to actors.name. Both go
        // through sanitizeLabel since DB-controlled values can carry
        // brackets/newlines that would break the line shape.
        const speaker = sanitizeLabel(row.from_display_name || row.from_actor_name);
        const text = sanitizeSpeech(row.message || '');
        if (!text) {
            return null;
        }
        return {
            at: new Date(row.sent_at),
            line: '[' + formatTimestamp(row.sent_at) + ' ' + speaker + '] "' + text + '"',
        };
    }).filter(Boolean);

    // Merge and sort. Stable order for same-timestamp entries: actions
    // before speech (engine commits then chat reflects them; reversing
    // would imply the speech preceded the move which doesn't match
    // engine semantics).
    const all = [
        ...narrationLines.map((x) => ({ ...x, kindOrder: 0 })),
        ...speechLines.map((x) => ({ ...x, kindOrder: 1 })),
    ];
    all.sort((a, b) => {
        const tdiff = a.at - b.at;
        if (tdiff !== 0) {
            return tdiff;
        }
        return a.kindOrder - b.kindOrder;
    });

    const slug = 'conversations/' + dayStr + '-sim-day';
    const title = 'Sim day — ' + actorName + ' — ' + dayStr;

    if (all.length === 0) {
        // No events and no speech — nothing happened (or nothing was
        // pushed). Skip writing rather than producing an empty note;
        // the dream cron will simply not see this day for this agent.
        logSim('skip-empty', { agent: agentName, day: dayStr });
        return { skipped: true, reason: 'no events or speech' };
    }

    const headerLines = [
        'Day: ' + dayStr + ' — ' + actorName,
        '',
    ];
    const content = headerLines.concat(all.map((x) => x.line)).join('\n') + '\n';

    await saveNote(agentName, title, content, slug, agentName, null, null, { upsert: true });
    logSim('saved', { agent: agentName, day: dayStr, lines: all.length, bytes: content.length });
    return { saved: true, slug, lines: all.length, bytes: content.length };
}

module.exports = { distillSimConversationDay };
