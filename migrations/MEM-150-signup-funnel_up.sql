-- MEM-150: durable signup-funnel facts per account (LLM-734).
--
-- The 10-09 funnel read (outside accounts since May: 13 created -> 8 connected
-- -> 6 saved a note -> 0 still active) had to be pieced together from proxies.
-- request_log holds MCP tool calls but is purged after 7 days
-- (scripts/db-cleanup.sh), so "when did this account first use a tool" is lost
-- for anyone older than a week, and nothing records which client a user picked
-- on the signup guide.
--
-- 1. signup_events — what a new account did on the "You're in!" guide
--    (register.html): the guide shown (with the tab open at the time), a client
--    tab picked, a copy button pressed. Insert-only, so the order and the time
--    show how far a person got. Written by POST /v1/agent/signup-event, which
--    accepts events only in an account's first 24 hours.
--
-- 2. actors.first_connected_at — first authenticated /mcp request (stamped in
--    the MCP auth heartbeat). actors.first_tool_call_at / last_tool_call_at /
--    tool_call_count — MCP tools/call only (initialize and tools/list do not
--    count: claude.ai repeats them about every 2 hours with no chat).
--
-- Backfill: request_log reaches back 7 days only, so it can seed the LAST tool
-- call and a partial count, never the first. first_* stay NULL for accounts
-- older than the log; a wrong first date would read as fact.
--
-- The same reasoning keeps first_* NULL for those accounts FOREVER: an old
-- account that connects next week must not get next week as its "first".
-- config.signup_funnel_since records when tracking began; the stamps set
-- first_* only for an account created at or after it (services/signup-funnel.js).

BEGIN;

CREATE TABLE signup_events (
    id          BIGSERIAL PRIMARY KEY,
    actor_id    INTEGER NOT NULL REFERENCES actors(id) ON DELETE CASCADE,
    event       TEXT NOT NULL,
    value       TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT signup_events_event_check CHECK (event IN ('guide_shown', 'client', 'copy'))
);

CREATE INDEX idx_signup_events_actor ON signup_events (actor_id, created_at);

ALTER TABLE actors
    ADD COLUMN first_connected_at TIMESTAMPTZ,
    ADD COLUMN first_tool_call_at TIMESTAMPTZ,
    ADD COLUMN last_tool_call_at TIMESTAMPTZ,
    ADD COLUMN tool_call_count INTEGER NOT NULL DEFAULT 0;

INSERT INTO config (key, value, description) VALUES
    -- Microseconds kept: truncating to the second would move the boundary back
    -- and let an account created earlier in that second count as tracked.
    ('signup_funnel_since', to_char(NOW() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
     'When per-account first-connection / first-tool-call tracking began (MEM-150). Accounts created earlier never get first_* stamps, since their true first use is unknown. Do not edit.');

-- request_log.path is '/mcp → <tool>' for a tools/call (request-log.js) and
-- '/mcp → <method>' for every other JSON-RPC method. Tool names never hold a
-- '/', and every other method does (tools/list, notifications/initialized,
-- server/discover …) except initialize and ping.
WITH calls AS (
    SELECT actor_id, MAX(timestamp) AS last_call, COUNT(*) AS calls
    FROM request_log
    WHERE actor_id IS NOT NULL
      AND path LIKE '/mcp → %'
      AND path NOT LIKE '/mcp → %/%'
      AND path NOT IN ('/mcp → initialize', '/mcp → ping')
    GROUP BY actor_id
)
UPDATE actors a
SET last_tool_call_at = c.last_call,
    tool_call_count = c.calls
FROM calls c
WHERE c.actor_id = a.id;

COMMIT;
