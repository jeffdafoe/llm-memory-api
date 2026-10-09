-- MEM-150 down: drop the signup-funnel table and the per-account usage stamps.

BEGIN;

DROP TABLE signup_events;

DELETE FROM config WHERE key = 'signup_funnel_since';

ALTER TABLE actors
    DROP COLUMN first_connected_at,
    DROP COLUMN first_tool_call_at,
    DROP COLUMN last_tool_call_at,
    DROP COLUMN tool_call_count;

COMMIT;
