// Signup funnel (LLM-734, migration MEM-150) — durable per-account facts that
// answer: which client did a new user pick, did they connect, when did they
// first use a tool, and are they still using it. request_log cannot answer the
// last three past its 7-day purge, so the stamps live on actors.
//
// Three writers and one reader:
//   - stampConnected: the MCP auth heartbeat folds it into its own UPDATE
//     (connectedSql) — first authenticated /mcp request.
//   - stampToolCall: the MCP tools/call handler, once per call.
//   - recordSignupEvent: POST /v1/agent/signup-event from the signup guide.
//   - funnelRows / funnelSummary: GET-side for the admin Access panel.
//
// first_* stamps are set only for accounts created at or after
// config.signup_funnel_since. An older account's true first use is unknown,
// and stamping its next use as "first" would read as fact.

const pool = require('../db');
const config = require('./config');

// The tracking start as an ISO string, or null when the config row is missing
// (then no first_* stamp is ever written — fail closed).
function trackingSince() {
    const value = config.get('signup_funnel_since');
    if (!value || Number.isNaN(Date.parse(value))) {
        return null;
    }
    return value;
}

// SET-clause fragment for the MCP heartbeat UPDATE. $2 is trackingSince().
// A NULL $2 compares as NULL, so the CASE never fires.
const connectedSql = `first_connected_at = COALESCE(first_connected_at,
                 CASE WHEN created_at >= $2::timestamptz THEN NOW() END)`;

// Fire-and-forget: a lost stamp must never fail the tool call it describes.
function stampToolCall(actorId) {
    if (!actorId) {
        return;
    }
    pool.query(
        `UPDATE actors
         SET first_tool_call_at = COALESCE(first_tool_call_at,
                 CASE WHEN created_at >= $2::timestamptz THEN NOW() END),
             last_tool_call_at = NOW(),
             tool_call_count = tool_call_count + 1
         WHERE id = $1`,
        [actorId, trackingSince()]
    ).catch((err) => {
        console.error('signup-funnel: tool-call stamp failed:', err.message);
    });
}

// Events the signup guide may send. value names a client tab or a copy button;
// the page owns those names, so only their shape is checked here.
const SIGNUP_EVENTS = ['guide_shown', 'client', 'copy'];
const VALUE_PATTERN = /^[a-z][a-z0-9_-]{0,31}$/;
// The guide shows once, right after signup. Past this window an event is not a
// signup fact, so it is dropped rather than stored.
const SIGNUP_WINDOW_HOURS = 24;
// A page left open could send copy clicks without end; this bounds the rows.
const MAX_EVENTS_PER_ACCOUNT = 100;
// First key of the two-key advisory lock that serializes one account's event
// inserts (the second key is the actor id). Any fixed int4 unique to this use.
const SIGNUP_EVENT_LOCK_CLASS = 734001;

function badRequest(message) {
    const err = new Error(message);
    err.statusCode = 400;
    return err;
}

// Store one guide event for actorId. Returns { recorded: true } or
// { recorded: false, reason } when the account is past its window or cap —
// not an error, since the page cannot know either and must not show one.
async function recordSignupEvent(actorId, event, value) {
    if (!SIGNUP_EVENTS.includes(event)) {
        throw badRequest('event must be one of: ' + SIGNUP_EVENTS.join(', '));
    }
    if (typeof value !== 'string' || !VALUE_PATTERN.test(value)) {
        throw badRequest('value must be 1-32 lowercase letters, digits, hyphens or underscores, starting with a letter');
    }
    // Concurrent inserts for one account would each count the same rows and all
    // pass the cap, so they are serialized per account: a transaction-scoped
    // advisory lock (released at COMMIT/ROLLBACK), then the count-and-insert as
    // a fresh statement. Under READ COMMITTED that statement's snapshot is taken
    // after the lock, so it sees every row the previous holder committed. An
    // advisory lock rather than FOR UPDATE on the actor row, because the MCP
    // heartbeat updates that row on every request.
    const client = await pool.connect();
    try {
        await client.query('BEGIN');
        await client.query('SELECT pg_advisory_xact_lock($1, $2)', [SIGNUP_EVENT_LOCK_CLASS, actorId]);
        const result = await client.query(
            `INSERT INTO signup_events (actor_id, event, value)
             SELECT a.id, $2, $3
             FROM actors a
             WHERE a.id = $1
               AND a.created_at > NOW() - make_interval(hours => $4)
               AND (SELECT COUNT(*) FROM signup_events s WHERE s.actor_id = a.id) < $5
             RETURNING id`,
            [actorId, event, value, SIGNUP_WINDOW_HOURS, MAX_EVENTS_PER_ACCOUNT]
        );
        await client.query('COMMIT');
        if (result.rows.length === 0) {
            return { recorded: false, reason: 'outside the signup window or over the event cap' };
        }
        return { recorded: true };
    } catch (err) {
        // A failed ROLLBACK must not hide the original error.
        await client.query('ROLLBACK').catch(() => {});
        throw err;
    } finally {
        client.release();
    }
}

// Outside accounts: self-registered people, not the owner, the salem sim
// accounts or virtual agents (Jeff, 2026-10-09).
const OUTSIDE_ACCOUNTS_SQL = `a.created_by = a.id
           AND NOT agc.virtual
           AND a.name <> 'jeff'
           AND a.name NOT LIKE 'zbbs-%'`;

// One row per outside account, newest first. client = the last tab picked, or
// the tab the guide opened on when none was picked. "connected" is
// first_connected_at for a tracked account. An account created before tracking
// falls back to API-key use (an OAuth token exchange or a direct key use both
// stamp agent_api_keys.last_used_at). The fallback is NOT used for tracked
// accounts: the signup guide's own events authenticate with the new key, so
// every tracked account has a key use before it ever connects a client.
async function funnelRows() {
    const result = await pool.query(
        `SELECT a.id, a.name, a.email, a.created_at,
                a.first_connected_at, a.first_tool_call_at, a.last_tool_call_at, a.tool_call_count,
                (a.created_at >= $1::timestamptz) AS tracked,
                ev.picked_client, ev.shown_client, ev.copies, ev.guide_shown_at,
                keys.key_last_used_at
         FROM actors a
         JOIN agent_configuration agc ON agc.actor_id = a.id
         LEFT JOIN LATERAL (
             SELECT (ARRAY_AGG(value ORDER BY created_at DESC) FILTER (WHERE event = 'client'))[1] AS picked_client,
                    (ARRAY_AGG(value ORDER BY created_at) FILTER (WHERE event = 'guide_shown'))[1] AS shown_client,
                    MIN(created_at) FILTER (WHERE event = 'guide_shown') AS guide_shown_at,
                    COALESCE(ARRAY_AGG(DISTINCT value) FILTER (WHERE event = 'copy'), '{}') AS copies
             FROM signup_events s
             WHERE s.actor_id = a.id
         ) ev ON TRUE
         LEFT JOIN LATERAL (
             SELECT MAX(k.last_used_at) AS key_last_used_at
             FROM agent_api_keys k
             WHERE k.actor_id = a.id
         ) keys ON TRUE
         WHERE ${OUTSIDE_ACCOUNTS_SQL}
         ORDER BY a.created_at DESC`,
        [trackingSince()]
    );
    return result.rows.map((row) => {
        const client = row.picked_client || row.shown_client || null;
        const tracked = row.tracked === true;
        return {
            name: row.name,
            email: row.email,
            created_at: row.created_at,
            tracked,
            guide_shown_at: row.guide_shown_at,
            client,
            client_picked: Boolean(row.picked_client),
            copies: row.copies || [],
            connected: Boolean(row.first_connected_at || (!tracked && row.key_last_used_at)),
            first_connected_at: row.first_connected_at,
            key_last_used_at: row.key_last_used_at,
            first_tool_call_at: row.first_tool_call_at,
            last_tool_call_at: row.last_tool_call_at,
            tool_call_count: row.tool_call_count,
            active_after_7_days: activeAfter(row, 7),
            active_after_30_days: activeAfter(row, 30)
        };
    });
}

// True when the account called a tool at least `days` after it was created.
// null when the account is not yet `days` old — not active, not lapsed, unknown.
function activeAfter(row, days) {
    const created = new Date(row.created_at).getTime();
    const threshold = created + days * 86400000;
    if (Date.now() < threshold) {
        return null;
    }
    return Boolean(row.last_tool_call_at) && new Date(row.last_tool_call_at).getTime() >= threshold;
}

// Stage counts over funnelRows(). The 7/30-day stages count only accounts old
// enough to have reached them, and say how many that is.
function funnelSummary(rows) {
    const oldEnough = (days) => rows.filter((r) => (days === 7 ? r.active_after_7_days : r.active_after_30_days) !== null);
    const seven = oldEnough(7);
    const thirty = oldEnough(30);
    return {
        signed_up: rows.length,
        saw_guide: rows.filter((r) => r.guide_shown_at).length,
        picked_client: rows.filter((r) => r.client_picked).length,
        connected: rows.filter((r) => r.connected).length,
        first_tool_call: rows.filter((r) => r.first_tool_call_at || r.tool_call_count > 0).length,
        active_after_7_days: seven.filter((r) => r.active_after_7_days).length,
        old_enough_for_7_days: seven.length,
        active_after_30_days: thirty.filter((r) => r.active_after_30_days).length,
        old_enough_for_30_days: thirty.length
    };
}

module.exports = {
    trackingSince,
    connectedSql,
    stampToolCall,
    recordSignupEvent,
    funnelRows,
    funnelSummary,
    activeAfter,
    SIGNUP_EVENTS,
    SIGNUP_WINDOW_HOURS,
    MAX_EVENTS_PER_ACCOUNT
};
