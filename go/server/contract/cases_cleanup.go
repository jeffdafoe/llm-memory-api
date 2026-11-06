package contract

// cleanupCases run the cleanup job (decay cleanup, the call-log purge,
// conversation retention) once on each server's database copy. A setup step
// adds the rows each task acts on in a namespace of their own, and sets
// cleanup_cron_schedule, which the fixture leaves empty so neither server
// schedules it. The job's log lines are the step's body; its writes are in
// the tables.
var cleanupCases = []Scenario{
	{
		Name:  "cleanup pass",
		Steps: []Step{{SQL: cleanupSetup}, {Job: "cleanup"}},
	},
	{
		// A second pass finds nothing new: the purged_at and deleted_at
		// guards keep it from touching the rows the first pass retired.
		Name:  "cleanup pass twice",
		Steps: []Step{{SQL: cleanupSetup}, {Job: "cleanup"}, {Job: "cleanup"}},
	},
	{
		// The purge's interval overflows in Postgres; the failure is logged
		// and recorded in error_log, and the other tasks still run (here
		// both are off).
		Name: "cleanup pass with a failing purge",
		Steps: []Step{
			{SQL: cleanupSetup + `
UPDATE config SET value = 'false' WHERE key = 'cleanup_enabled';
UPDATE config SET value = '99999999999999999999999' WHERE key = 'va_call_log_retention_days';
UPDATE config SET value = 'abc' WHERE key = 'conversation_retention_days';`},
			{Job: "cleanup"},
		},
	},
	{
		// Every half-life set to 0 except reflective; a threshold that
		// parseFloat reads as 0.5; retention 0 (one day).
		Name: "cleanup pass with other config",
		Steps: []Step{
			{SQL: cleanupSetup + `
UPDATE config SET value = '0' WHERE key LIKE 'search_decay_halflife_%';
UPDATE config SET value = '1' WHERE key = 'search_decay_halflife_reflective';
UPDATE config SET value = ' 0.5 days' WHERE key = 'cleanup_decay_threshold';
UPDATE config SET value = '0' WHERE key = 'conversation_retention_days';
UPDATE config SET value = '2' WHERE key = 'va_call_log_retention_days';
UPDATE documents SET metadata = '{"cognitive_type": "Reflective"}' WHERE namespace = 'cleanup-ns' AND slug = 'notes/accessed';`},
			{Job: "cleanup"},
		},
	},
}

// cleanupSetup is the same text on both copies. Ages are far from every
// boundary, so the two servers, running a few milliseconds apart, see the
// same rows on each side of each cut.
const cleanupSetup = `
UPDATE config SET value = '0 5 * * *' WHERE key = 'cleanup_cron_schedule';
INSERT INTO namespace_usage (namespace, note_count, total_bytes) VALUES ('cleanup-ns', 9, 1000);
INSERT INTO documents (namespace, slug, title, content, kind, metadata, created_at, updated_at, last_accessed, deleted_at) VALUES
    ('cleanup-ns', 'learnings/stale', 'Stale', 'an old learning', 'learning', NULL,
        NOW() - INTERVAL '3000 days', NOW() - INTERVAL '3000 days', NULL, NULL),
    ('cleanup-ns', 'notes/episode', 'Episode', 'an old episode', 'note', '{"cognitive_type": " Episodic "}',
        NOW() - INTERVAL '3000 days', NOW() - INTERVAL '3000 days', NOW() - INTERVAL '2000 days', NULL),
    ('cleanup-ns', 'notes/accessed', 'Accessed', 'old but read this week', 'note', NULL,
        NOW() - INTERVAL '3000 days', NOW() - INTERVAL '3000 days', NOW() - INTERVAL '2 days', NULL),
    ('cleanup-ns', 'notes/procedure', 'Procedure', 'an old procedure', 'note', '{"cognitive_type": "procedural"}',
        NOW() - INTERVAL '3000 days', NULL, NULL, NULL),
    ('cleanup-ns', 'notes/codebase/ref', 'Ref', 'an old reference', 'reference', NULL,
        NOW() - INTERVAL '3000 days', NOW() - INTERVAL '3000 days', NULL, NULL),
    ('cleanup-ns', 'conversations/old-session', 'Old', 'an old session', 'conversation', '{"session_date": "2020-01-01"}',
        NOW(), NOW(), NULL, NULL),
    ('cleanup-ns', 'conversations/recent', 'Recent', 'a recent session', 'conversation',
        jsonb_build_object('session_date', (NOW() - INTERVAL '2 days')::date::text), NOW(), NOW(), NULL, NULL),
    ('cleanup-ns', 'conversations/bad-date', 'Bad', 'a session with a bad date', 'conversation', '{"session_date": "2020-13-45"}',
        NOW() - INTERVAL '40 days', NOW() - INTERVAL '40 days', NULL, NULL),
    ('cleanup-ns', 'conversations/retired', 'Retired', 'retired long ago', 'conversation', '{"session_date": "2020-01-01"}',
        NOW() - INTERVAL '400 days', NOW() - INTERVAL '400 days', NULL, NOW() - INTERVAL '100 days'),
    ('cleanup-ns', 'conversations/retired-recently', 'Retired recently', 'retired this week', 'conversation', NULL,
        NOW() - INTERVAL '400 days', NOW() - INTERVAL '400 days', NULL, NOW() - INTERVAL '5 days');
INSERT INTO memory_chunks (namespace, source_file, heading, chunk_text) VALUES
    ('cleanup-ns', 'learnings/stale', NULL, 'an old learning'),
    ('cleanup-ns', 'Notes/Episode', NULL, 'an old episode'),
    ('cleanup-ns', 'notes/accessed', NULL, 'old but read this week'),
    ('cleanup-ns', 'conversations/old-session', NULL, 'an old session'),
    ('cleanup-ns', 'conversations/recent', NULL, 'a recent session'),
    ('cleanup-ns', 'conversations/retired', NULL, 'retired long ago');
INSERT INTO virtual_agent_calls (actor_id, provider, model, created_at)
    SELECT id, 'anthropic', 'claude-x', NOW() - age
    FROM actors, (VALUES (INTERVAL '10 days'), (INTERVAL '3 days'), (INTERVAL '1 hour')) AS ages(age)
    WHERE name = 'agent-a';
-- Sim chat for the retention purge (sim_chat_retention_days, 14 in seed.sql):
-- the two old texts the engine sent or was sent go; the recent one, the old
-- engine line in a discussion, and old chat between other agents stay.
WITH t AS (
    INSERT INTO chat_message_texts (message, from_actor_id, discussion_id, sent_at)
    SELECT v.message, f.id, v.discussion_id, NOW() - v.age
    FROM (VALUES
        ('old engine tick', 'salem-engine', NULL::int, INTERVAL '20 days'),
        ('old reply to the engine', 'agent-a', NULL::int, INTERVAL '20 days'),
        ('recent engine tick', 'salem-engine', NULL::int, INTERVAL '2 days'),
        ('old engine discussion line', 'salem-engine', 101, INTERVAL '20 days'),
        ('old agent chat', 'agent-a', NULL::int, INTERVAL '20 days')
    ) AS v(message, sender, discussion_id, age)
    JOIN actors f ON f.name = v.sender
    RETURNING id, message
)
INSERT INTO chat_messages (message_text_id, to_actor_id)
SELECT t.id, r.id
FROM t
JOIN (VALUES
    ('old engine tick', 'agent-a'), ('old reply to the engine', 'salem-engine'), ('recent engine tick', 'agent-a'),
    ('old engine discussion line', 'agent-a'), ('old agent chat', 'agent-b')
) AS d(message, recipient) ON d.message = t.message
JOIN actors r ON r.name = d.recipient;
`
