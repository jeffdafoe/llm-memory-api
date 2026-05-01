// Sim-conversation distiller — daily per-NPC narrative builder.
//
// Receives a typed event payload from salem-engine (one push per sim NPC
// per just-completed in-sim day) and produces a single
// conversations/YYYY-MM-DD-sim-day note in the agent's namespace,
// formatted as `[Day HH:MM Speaker] text` lines.
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
// The logTranscript path skips dream_mode='sim' agents entirely; this
// distiller is the only thing writing conversations/* notes for them.
// chat_message_texts is still written for audit/replay but is NOT joined
// here — for sim NPCs that table holds only chat-completion plumbing
// (system prompts the engine sent to the model, tool-result acks, JSON
// tool_call responses), none of which is narrative speech. The engine
// records every narrative action — including speak/act with the actual
// text — into agent_action_log and forwards them in the daily push, so
// that single payload is the sole source of truth here.
//
// Cross-actor speech: the engine push (post-ZBBS-094) includes other
// actors' speak/act rows when those actors shared a scene_huddle with
// the target NPC. The target's own note thereby contains "[19:00 John
// Ellis] 'Another round?'" alongside "[19:01 Jefferey] 'Aye'", which is
// what makes the day usable as dream input. Each event carries a
// `speaker` field used directly for the line label.
//
// Engine event shape — caller sends an array of:
//   { at: ISO timestamp, kind: action_type, payload: object,
//     speaker: display name }
// Kinds correspond to agent_action_log.action_type values: 'speak',
// 'act', 'move_to', 'chore', 'pay', 'object_refresh', etc. Unknown
// kinds get a generic narration so a new engine action_type doesn't
// silently drop frames.

const pool = require('../db');
const { saveNote } = require('./documents');
const { log, logError } = require('./logger');

function logSim(event, payload) {
    log('sim-distill', { event, ...payload });
}

// Format a YYYY-MM-DD day string into a UTC window [00:00, next 00:00).
// Engine pushes the just-completed day; this is the boundary the
// per-event filter uses to drop any rows that arrived outside it.
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

// Derive a human-readable display label from an actor slug. The api's
// actors table only stores the slug (e.g. zbbs-john-ellis); the canonical
// display_name lives on Salem's own actor table in the engine DB, which
// we can't query from here. Inverts the convention loadPeopleContext
// already uses going the other way ("Josiah Thorne" -> josiah-thorne):
// strip the leading zbbs- prefix, split on hyphens, title-case each
// segment, rejoin with spaces. Non-zbbs slugs (salem-chronicler, home,
// the engine itself) get title-cased as-is so they still render
// readably if they ever appear as a speaker.
function slugToDisplay(name) {
    if (!name) {
        return '';
    }
    const stripped = String(name).replace(/^zbbs-/, '');
    return stripped
        .split('-')
        .filter(Boolean)
        .map((w) => w.charAt(0).toUpperCase() + w.slice(1))
        .join(' ');
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

// Sanitize narration prose for paren-wrapped action text ("poured ale
// for Jefferey, Wendy, and Ezekiel Crane"). Collapses whitespace so a
// multi-line verb_phrase stays on one transcript line. Unlike
// sanitizeLabel, leaves brackets and quotes alone — they don't break
// the line shape inside parens, and stripping them mangles natural
// prose punctuation.
function sanitizeNarration(s) {
    return String(s || '')
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
        case 'speak': {
            // payload.text is the spoken line, copied verbatim from the
            // model's tool-call args at engine/agent_tick.go's "speak"
            // case. Render as a quoted string (not parens) so dialogue
            // and narration are visually distinct in the transcript —
            // matches the work-agent distillation shape.
            const text = sanitizeSpeech(p.text || '');
            if (!text) {
                return null;
            }
            return '"' + text + '"';
        }
        case 'act': {
            // payload.verb_phrase is a short third-person physical
            // action ("poured ale for Jefferey, Wendy, and Ezekiel
            // Crane"), recorded so other co-located NPCs can perceive
            // it next tick. Not dialogue — render in parens as
            // narration alongside move_to, chore, pay. sanitizeNarration
            // (rather than sanitizeLabel) preserves natural prose
            // punctuation; brackets/quotes inside parens don't break
            // the line shape.
            const verb = sanitizeNarration(p.verb_phrase || '');
            if (!verb) {
                return null;
            }
            return '(' + verb + ')';
        }
        case 'enter_huddle':
            // Membership marker (ZBBS-094). The engine writes one of
            // these whenever an actor's current_huddle_id is updated,
            // so loadDayEvents' my_huddles CTE can discover huddles
            // even for actors who join silently and never speak. The
            // row drives membership, not narration — return null so
            // it doesn't produce a transcript line.
            return null;
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

// Resolve the agent name to (actor_id, name, dream_mode). The
// distiller is sim-only — return null if dream_mode != 'sim' so the
// caller can reject without producing an empty note.
async function resolveSimAgent(agentName) {
    const r = await pool.query(
        `SELECT ac.id, ac.name, agc.dream_mode
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
    const actorName = sanitizeLabel(slugToDisplay(agent.name));

    // Build per-event narration lines from the engine push. Skip events
    // that fall outside the day window — the engine should only push
    // events for the requested day, but defend against off-by-one.
    //
    // event.speaker is the agent_action_log.speaker_name on the engine
    // side. Always populated post-ZBBS-094. For the target agent's own
    // events it equals actorName; for cross-actor speak/act pulled via
    // shared scene_huddle membership it's the OTHER speaker (Ezekiel,
    // Jefferey, etc.) and using it as the line label is the whole point
    // of the cross-actor pull. Sanitize since speaker_name is
    // model/PC-supplied and could contain bracket-breaking characters.
    //
    // seq is a monotonically increasing index used as a stable
    // tiebreak when sorting. Two rows can share occurred_at down to the
    // millisecond when a cascade fans out to several actors near-
    // simultaneously; the engine query orders by (occurred_at, id) for
    // SQL determinism, and we preserve that ordering on the api side
    // by carrying the push-array index alongside.
    const narrationLines = [];
    let seq = 0;
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
        const lineSpeaker = event.speaker ? sanitizeLabel(event.speaker) : actorName;
        narrationLines.push({
            at,
            seq,
            line: '[' + formatTimestamp(event.at) + ' ' + lineSpeaker + '] ' + text,
        });
        seq += 1;
    }

    // Sort defensively — the engine query already returns rows
    // ORDER BY occurred_at ASC, al.id ASC, but a future change to the
    // push payload shouldn't silently reorder the transcript. seq
    // breaks ties so co-timestamped cascade rows keep the engine's
    // deterministic order rather than shuffling between pushes.
    narrationLines.sort((a, b) => {
        const tdiff = a.at - b.at;
        if (tdiff !== 0) {
            return tdiff;
        }
        return a.seq - b.seq;
    });
    const all = narrationLines;

    const slug = 'conversations/' + dayStr + '-sim-day';
    const title = 'Sim day — ' + actorName + ' — ' + dayStr;

    if (all.length === 0) {
        // Nothing happened (or nothing narratable was pushed — a day of
        // pure look_around/done collapses to zero lines). Skip writing
        // rather than producing an empty note; the dream cron will
        // simply not see this day for this agent.
        logSim('skip-empty', { agent: agentName, day: dayStr });
        return { skipped: true, reason: 'no narratable events' };
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
