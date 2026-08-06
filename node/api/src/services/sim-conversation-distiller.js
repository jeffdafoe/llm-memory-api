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
// actors' speak (v1) / spoke (v2) rows when those actors shared a
// scene_huddle with the target NPC. The target's own note thereby contains "[19:00 John
// Ellis] 'Another round?'" alongside "[19:01 Jefferey] 'Aye'", which is
// what makes the day usable as dream input. Each event carries a
// `speaker` field used directly for the line label.
//
// Engine event shape — caller sends an array of:
//   { at: ISO timestamp, kind: action_type, payload: object,
//     speaker: display name }
// Kinds correspond to agent_action_log.action_type values. The v1 engine
// emitted 'speak' / 'pay' / 'move_to' / 'act' / 'chore' / 'object_refresh';
// the v2 rewrite renamed the verbs and added a few, so narrateEvent also
// handles 'spoke' / 'paid' / 'walked' / 'delivered' / 'consumed' /
// 'took_break' (ZBBS-WORK-376) / 'labored' (LLM-162) / 'solicited_work' /
// 'hired' (LLM-213) / 'offered_work' (LLM-564). Unknown kinds get a generic
// narration so a new engine action_type doesn't silently drop frames.

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
// segment, rejoin with spaces. Non-zbbs slugs (salem-generic, home,
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

// Format a coin amount as "N coins" (or "1 coin"). Coerces to a
// non-negative integer first — a string-shaped amount in the engine payload
// could otherwise inject bracket/newline characters into the transcript line.
// Shared by the pay/paid and delivered cases.
function formatCoins(amount) {
    const raw = Number(amount);
    const n = Number.isFinite(raw) && raw > 0 ? Math.floor(raw) : 0;
    return n === 1 ? '1 coin' : n + ' coins';
}

// Format an item kind + quantity as "ale" (qty <= 1) or "3x bread" (qty > 1),
// mirroring the engine-side digest shape. Shared by the delivered and consumed
// cases.
function formatItemQty(item, qty) {
    const name = sanitizeLabel(item || 'something');
    const n = Number(qty);
    if (Number.isFinite(n) && n > 1) {
        return Math.floor(n) + 'x ' + name;
    }
    return name;
}

// Format a labor reward's payment terms (LLM-225): coins, in-kind goods
// (payload.reward_items — [{item, qty}], present only when the hire's pay
// carries goods), or both. Coins-only payloads render exactly as before
// ("5 coins"), so pre-LLM-225 rows and coin hires are unchanged; an in-kind
// leg joins with commas/"and" ("porridge and 2 coins"). A goods-only reward
// (amount 0) drops the coin leg rather than rendering "and 0 coins". Each
// goods line goes through formatItemQty, so item names are sanitized the
// same way the delivered/consumed lines are. Shared by the labored /
// solicited_work / hired cases.
function formatLaborReward(p) {
    const parts = [];
    if (Array.isArray(p.reward_items)) {
        for (const line of p.reward_items) {
            if (!line || typeof line !== 'object') {
                continue;
            }
            // Validate the line before pushing (code_review): an empty/
            // sanitized-away item name or a non-positive/non-numeric qty must
            // be SKIPPED, not rendered — otherwise a malformed line both
            // contributes junk text ("something") and suppresses the
            // coins-only fallback below. formatItemQty still does the
            // rendering (and its own sanitize) on the raw name.
            const item = sanitizeLabel(line.item || '');
            const qty = Number(line.qty);
            if (!item || !Number.isInteger(qty) || qty < 1) {
                continue;
            }
            parts.push(formatItemQty(line.item, line.qty));
        }
    }
    const coins = Number(p.amount);
    // Keep the coin leg whenever it is positive, and also as the sole
    // fallback when there are no goods lines — an all-empty reward then
    // renders "0 coins", matching the pre-LLM-225 behavior for malformed
    // payloads instead of producing an empty phrase.
    if ((Number.isFinite(coins) && coins > 0) || parts.length === 0) {
        parts.push(formatCoins(p.amount));
    }
    if (parts.length === 1) {
        return parts[0];
    }
    return parts.slice(0, -1).join(', ') + ' and ' + parts[parts.length - 1];
}

// Tracks unmapped action_types already logged this process, so the default
// case's "needs a mapping" signal fires once per kind rather than once per row —
// a busy day of gathered / stayed_open rows would otherwise spam the log. Reset
// only on restart; a single line per kind is enough to prompt adding a mapping.
const loggedUnmappedKinds = new Set();

// Map an engine event to a narration line. Returns the (action) text
// rendered in parens, or null when the event carries no narrative
// signal (look_around, done with no state change). Keeping this in
// one place means a new action_type lands an entry once and shows up
// everywhere consistently.
function narrateEvent(event, actorName) {
    const p = event.payload || {};
    switch (event.kind) {
        // v1 'move_to' and v2 'walked' are the same action under two
        // action_type names (the v2 rewrite renamed the verbs). Both carry the
        // destination in payload.destination, so they share a case.
        case 'move_to':
        case 'walked': {
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
        // v1 'pay' and v2 'paid' — same buyer-side action, two names.
        case 'pay':
        case 'paid': {
            const recipient = sanitizeLabel(p.recipient || p.recipient_name || 'someone');
            // LLM-607: a payment that discharged the town rate is narrated as the
            // due it was, and the payer's own words for it are DROPPED.
            //
            // This line goes into the ledger block of the consolidation prompt,
            // under a directive telling the model the ledger is authoritative and
            // complete. `payload.for` is model-authored — the villager's stated
            // purpose — and for a levy it reads "Day's rate on the James Farm",
            // which is a payment with a stated purpose and no delivery against it,
            // i.e. indistinguishable from an order placed and never filled. Nine of
            // those consolidated into Moses James's character document as a standing
            // grievance ("takes my coin ... but never delivers"), and the constable
            // refunded eight coins of collected rate on the strength of it.
            //
            // The engine settled the rate and knows what the coin was; the salem
            // side stamps that on the durable payload as rate_settled. Its presence
            // is the classification. Preferring it over the payer's account is the
            // same rule the engine already applies to its own relationship facts.
            const rateSettled = Number(p.rate_settled);
            if (Number.isFinite(rateSettled) && rateSettled > 0) {
                return '(paid ' + recipient + ' ' + formatCoins(p.amount)
                    + ' — the town rate, a due settled, with no goods owed in return)';
            }
            const forText = sanitizeLabel(p.for || p.for_text || '');
            const reason = forText ? ' for ' + forText : '';
            return '(paid ' + recipient + ' ' + formatCoins(p.amount) + reason + ')';
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
        // v1 'speak' and v2 'spoke' — same action, two names. payload.text is
        // the spoken line, copied verbatim from the model's speak tool-call.
        // Rendered as a quoted string (not parens) so dialogue and narration
        // are visually distinct in the transcript — matches the work-agent
        // distillation shape. This is also the only cross-actor kind the v2
        // push forwards (a huddle-mate's overheard speech).
        case 'speak':
        case 'spoke': {
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
        // v2-only action_types (ZBBS-WORK-376) — no v1 narrateEvent equivalent.
        // The v2 engine records these committed actions in agent_action_log and
        // the daily push forwards them here.
        case 'delivered': {
            // Seller side of a fulfilled order (deliver_order tool). The buyer
            // logs their own 'paid' row; this is the seller's "handed the goods
            // over" record. payload: { recipient (buyer's name), item, qty,
            // amount }. The sale price is included when present.
            const recipient = sanitizeLabel(p.recipient || 'someone');
            const goods = formatItemQty(p.item, p.qty);
            const coins = Number(p.amount) > 0 ? ' for ' + formatCoins(p.amount) : '';
            return '(delivered ' + goods + ' to ' + recipient + coins + ')';
        }
        case 'consumed': {
            // consume tool — ate/drank an inventory item. payload: { item, qty }.
            // No food-vs-drink semantic is carried, so a neutral "had" reads
            // naturally for both ("had ale" / "had 2x bread").
            const goods = formatItemQty(p.item, p.qty);
            return '(had ' + goods + ')';
        }
        case 'took_break': {
            // take_break tool — the NPC stepped away from its post. payload:
            // { reason? } is the model-supplied prose ("weary from the day"),
            // rendered as a narration aside when present.
            const reason = sanitizeNarration(p.reason || '');
            return reason ? '(stepped away, ' + reason + ')' : '(stepped away)';
        }
        case 'labored': {
            // LLM-162: a completed solicit_work labor contract (settle-at-
            // completion). Worker-side row — the worker EARNED the reward
            // working for the employer. payload: { employer, amount,
            // duration_min }. The counterpart to the buyer-side 'paid' line;
            // duration is audit-only (recorded in agent_action_log) and left
            // out of the narration — the economic fact (who paid, how much) is
            // what the dream memory needs.
            // Fall back AFTER sanitizing: a blank/unsafe employer string
            // sanitizes to '' and would otherwise render "working for " — the
            // || 'someone' has to come last so it catches that case too.
            const employer = sanitizeLabel(p.employer || p.recipient || '') || 'someone';
            return '(earned ' + formatLaborReward(p) + ' working for ' + employer + ')';
        }
        case 'solicited_work': {
            // LLM-213: a worker offered to work for the employer (solicit_work
            // minted a live pending offer). Worker-side row. payload: { employer,
            // amount, duration_min }. The offer-time counterpart to the settle-
            // time 'labored' line; no coins move yet, so the beat is the ARRANGEMENT
            // (who, how much asked). duration is audit-only, left out of narration.
            // Same post-sanitize '|| someone' fallback as 'labored' so a blank/
            // unsafe employer still renders a clean line.
            const employer = sanitizeLabel(p.employer || p.recipient || '') || 'someone';
            return '(offered to work for ' + employer + ' for ' + formatLaborReward(p) + ')';
        }
        case 'offered_work': {
            // LLM-564: an employer offered a worker a job (offer_work minted a
            // live pending offer). Employer-side row — before this type existed
            // these mints logged as 'solicited_work' attributed to the worker,
            // reading in the log as the worker having asked. payload: { worker,
            // amount, duration_min } (the 'hired' shape). No coins move at the
            // offer; the beat is the arrangement proposed.
            const worker = sanitizeLabel(p.worker || p.recipient || '') || 'someone';
            return '(offered ' + worker + ' a job for ' + formatLaborReward(p) + ')';
        }
        case 'hired': {
            // LLM-213: an employer took a worker on (accept_work flipped the offer
            // to Working). Employer-side row. payload: { worker, amount,
            // duration_min }. No coins move at accept — the reward settles at
            // completion ('labored') — but the arrangement is the beat the dream
            // memory needs.
            const worker = sanitizeLabel(p.worker || p.recipient || '') || 'someone';
            return '(hired ' + worker + ' for ' + formatLaborReward(p) + ')';
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
            // A durable action_type with no narration mapping. Do NOT surface it
            // as generic "(Name kind)" text: loadDayEventsSQL pushes ALL of an
            // actor's durable rows regardless of type, so generic narration leaks
            // feed/audit-only beats into NPC dream memory. That is exactly how
            // gathered (LLM-273) and stayed_open (ZBBS-WORK-387) were already
            // producing "(Josiah gathered)" noise, and how LLM-283's offered /
            // declined / countered would too. Drop it from the dream transcript
            // and log ONCE per kind (loggedUnmappedKinds) so an unmapped kind
            // surfaces in server logs without spamming a line per row, and never
            // in production dreams. A beat that SHOULD feed dreams gets an
            // explicit case above.
            if (!loggedUnmappedKinds.has(event.kind)) {
                loggedUnmappedKinds.add(event.kind);
                log('sim-distill-unmapped-kind', { kind: event.kind });
            }
            return null;
    }
}

// Ceiling on the RAW prefix before canonicalization. Deliberately loose against
// the 100-char canonical cap: the raw value may carry surrounding whitespace and
// a run of trailing slashes that collapse away, so a tight bound here would
// reject values the canonical cap accepts. Its job is only to stop an absurd
// value from being processed at all.
const MAX_RAW_SLUG_PREFIX_LENGTH = 1024;

// collapseTrailingSlashes reduces a run of trailing slashes to exactly one.
//
// This is a linear scan rather than `replace(/\/+$/, '')`. That pattern is
// unanchored at the start, so on a run of slashes not followed by end-of-string
// the engine retries at every offset and backtracks in O(n^2) (CodeQL
// js/polynomial-redos). Measured on 200k slashes plus one character: 32,076ms
// for the regex against 0.005ms for this loop, same output.
//
// Exported only so the linearity guard can reach it. MAX_RAW_SLUG_PREFIX_LENGTH
// means normalizeSlugPrefix never hands this an adversarial input, so a test
// going through normalizeSlugPrefix would short-circuit at the length check and
// pass even with the quadratic regex restored — it has to call this directly to
// prove anything. The property matters independently of the bound: raising that
// ceiling must not silently reintroduce the backtracking.
function collapseTrailingSlashes(value) {
    let end = value.length;
    while (end > 0 && value[end - 1] === '/') {
        end--;
    }
    return value.slice(0, end) + '/';
}

// normalizeSlugPrefix validates and canonicalizes a shared-VA actor's memory
// partition prefix (e.g. 'constance-scott/') before it's spliced into the note
// slug. The engine derives it from the villager's display name (Slugify + '/'),
// so a legitimate value is one-or-more lowercase-kebab segments with a single
// trailing slash. Returning '' for anything else lets the caller reject with a
// 400 — this is the defense against a bad/hostile prefix injecting path
// traversal ('../') into the saved note's slug. Exported for unit tests.
function normalizeSlugPrefix(raw) {
    if (typeof raw !== 'string') {
        return '';
    }
    // Reject an oversized value before doing any work on it. The canonical form
    // is capped at 100 chars below, and the raw form can only legitimately add
    // surrounding whitespace and extra trailing slashes on top of that — so
    // anything past this ceiling was never going to be accepted. Bounding here
    // rather than at the end keeps the trim/slice/test off a value that arrives
    // on a 5 MB request body (express.json limit in server.js).
    if (raw.length > MAX_RAW_SLUG_PREFIX_LENGTH) {
        return '';
    }
    const trimmed = raw.trim();
    if (trimmed === '') {
        return '';
    }
    // Collapse trailing slashes to exactly one, then require a safe kebab path.
    const withSlash = collapseTrailingSlashes(trimmed);
    if (!/^[a-z0-9]+(-[a-z0-9]+)*\/$/.test(withSlash)) {
        return '';
    }
    // Bound the length: the prefix becomes part of the saved note slug and the
    // sim_shared_actor primary key, so an arbitrarily long (if well-formed) value
    // must not pass. 100 matches the ceiling other sim actor identifiers use
    // (routes/sim.js raw-turns sim_actor).
    if (withSlash.length > 100) {
        return '';
    }
    return withSlash;
}

// Resolve the agent name to (actor_id, name, dream_mode). The distiller is
// sim-only — return { _wrongMode } if dream_mode is neither 'sim' (a dedicated VA,
// one NPC per namespace) nor 'sim-shared' (a pooled VA whose villagers get
// per-actor notes under a slug prefix), so the caller can reject without
// producing an empty note.
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
    if (row.dream_mode !== 'sim' && row.dream_mode !== 'sim-shared') {
        return { ...row, _wrongMode: true };
    }
    return row;
}

// Main entry — receives an engine push, builds and saves the note.
// Idempotent: re-running the same (agent, day, events) overwrites the
// existing note. Returns a small summary for the caller.
async function distillSimConversationDay(agentName, dayStr, events, opts = {}) {
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
            new Error('agent ' + agentName + ' is not configured for a sim dream_mode'),
            { statusCode: 400 }
        );
    }

    // A 'sim-shared' agent pools many villagers, so each push is scoped to ONE of
    // them: it must carry that villager's memory-partition slug prefix (e.g.
    // 'constance-scott/') and display name. The note then lands under the actor's
    // subtree in the pooled namespace (salem-vendor/constance-scott/conversations/…,
    // the very prefix `recall` searches), and its lines are labeled with the
    // villager rather than the pooled agent. A dedicated 'sim' VA is one NPC per
    // namespace, so it carries neither — the note sits at the namespace root and is
    // labeled from the agent slug.
    const isShared = agent.dream_mode === 'sim-shared';
    let slugPrefix = '';
    let actorName;
    if (isShared) {
        slugPrefix = normalizeSlugPrefix(opts.slugPrefix);
        if (!slugPrefix) {
            throw Object.assign(
                new Error('slug_prefix required (safe kebab path) for a sim-shared agent'),
                { statusCode: 400 }
            );
        }
        const display = typeof opts.actorName === 'string' ? opts.actorName.trim() : '';
        if (!display) {
            throw Object.assign(
                new Error('actor (display name) required for a sim-shared agent'),
                { statusCode: 400 }
            );
        }
        // sanitizeLabel strips bracket/quote/newline chars and can collapse a
        // hostile input to empty; it imposes no length bound. Validate the RESULT
        // (not just the raw input) so an empty or oversized name can't reach the
        // note title/content or the roster display_name. 100 matches the slug cap.
        actorName = sanitizeLabel(display);
        if (!actorName || actorName.length > 100) {
            throw Object.assign(
                new Error('actor (display name) is empty after sanitizing, or too long, for a sim-shared agent'),
                { statusCode: 400 }
            );
        }
    } else {
        actorName = sanitizeLabel(slugToDisplay(agent.name));
    }

    const { start, end } = dayWindow(dayStr);

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

    const slug = slugPrefix + 'conversations/' + dayStr + '-sim-day';
    const title = 'Sim day — ' + actorName + ' — ' + dayStr;

    if (all.length === 0) {
        // Nothing happened (or nothing narratable was pushed — a day of
        // pure look_around/done collapses to zero lines). Skip writing
        // rather than producing an empty note; the dream cron will
        // simply not see this day for this agent. For a shared-VA push this
        // ALSO skips the sim_shared_actor enrollment below (which sits after this
        // return) — by design: the roster means "villagers with dreamable
        // material", so an actor with only empty days stays out of it until a day
        // lands a note (Slice 2 has nothing to consolidate for it anyway). Its
        // first non-empty day enrolls it.
        logSim('skip-empty', { agent: agentName, day: dayStr });
        return { skipped: true, reason: 'no narratable events' };
    }

    const headerLines = [
        'Day: ' + dayStr + ' — ' + actorName,
        '',
    ];
    const content = headerLines.concat(all.map((x) => x.line)).join('\n') + '\n';

    await saveNote(agentName, title, content, slug, agentName, null, null, { upsert: true });

    if (isShared) {
        // Register/refresh this villager in the shared-VA dream roster so Slice 2's
        // per-actor dream loop knows to consolidate salem-vendor/<slug_prefix>.
        // Keyed on (pooled agent, slug_prefix); the push is the authority for
        // display_name + last_pushed_day. INSERT-or-update is idempotent, matching
        // the note's own re-push-overwrites contract.
        await pool.query(
            `INSERT INTO sim_shared_actor (shared_actor_id, slug_prefix, display_name, last_pushed_day)
             VALUES ($1, $2, $3, $4)
             ON CONFLICT (shared_actor_id, slug_prefix)
             DO UPDATE SET display_name = EXCLUDED.display_name,
                           last_pushed_day = EXCLUDED.last_pushed_day`,
            [agent.id, slugPrefix, actorName, dayStr]
        );
    }

    logSim('saved', { agent: agentName, day: dayStr, lines: all.length, bytes: content.length });
    return { saved: true, slug, lines: all.length, bytes: content.length };
}

// narrateEvent is exported for unit tests (sim-conversation-distiller.test.js);
// distillSimConversationDay is the only production caller. slugToDisplay is
// shared with dream.js, which needs the same slug -> "First Last" rendering to
// recognize a dedicated NPC's own name in its distilled lines (LLM-523).
module.exports = { distillSimConversationDay, narrateEvent, normalizeSlugPrefix, slugToDisplay, collapseTrailingSlashes };
