-- MEM-145: conversation retention accounting (LLM-642).
--
-- Two one-time repairs behind the code change that moves conversation
-- retention out of scripts/db-cleanup.sh and into services/cleanup.js, plus
-- a config description that says what the key now governs.
--
-- 1. Re-uploaded conversations carry their re-upload date as created_at.
--    The old memory-sync diff hid deleted rows, so every session the 03:15
--    cron retired came back as a fresh row on the next sync (359 home and
--    363 work sessions on 2026-08-21 alone), each restarting its 31-day
--    retention clock. Put created_at back on the session's own date so
--    retention measures the conversation, not the upload. The dream cron
--    windows on created_at too, but the dates involved have already run, so
--    this re-feeds nothing. metadata is client supplied and unconstrained:
--    pg_input_is_valid (PG 16+) rejects a malformed or impossible date
--    without throwing, where a bare cast would abort the whole migration on
--    one bad row. CASE evaluates the cast only on the branch it takes.
UPDATE documents
SET created_at = (metadata->>'session_date')::date
WHERE kind = 'conversation'
  AND CASE WHEN pg_input_is_valid(metadata->>'session_date', 'date')
           THEN created_at::date > (metadata->>'session_date')::date
           ELSE false END;

-- 2. namespace_usage drifted far above reality (home: 51 MB counted against
--    18 MB live): the cron's soft-deletes never decremented it and every
--    re-upload added a whole note on top. Rebuild it from the live rows with
--    the LENGTH(content) measure saveNote and deleteNote use, and zero any
--    namespace that no longer has a live note.
INSERT INTO namespace_usage (namespace, note_count, total_bytes, updated_at)
SELECT namespace, COUNT(*), COALESCE(SUM(LENGTH(content)), 0), NOW()
FROM documents
WHERE deleted_at IS NULL
GROUP BY namespace
ON CONFLICT (namespace) DO UPDATE SET
    note_count = EXCLUDED.note_count,
    total_bytes = EXCLUDED.total_bytes,
    updated_at = NOW();

UPDATE namespace_usage u
SET note_count = 0, total_bytes = 0, updated_at = NOW()
WHERE NOT EXISTS (
    SELECT 1 FROM documents d WHERE d.namespace = u.namespace AND d.deleted_at IS NULL
);

-- 3. The description only ever mentioned the client-side window.
UPDATE config
SET description = 'Days of conversation history to keep. memory-sync offers sessions this recent; the nightly cleanup soft-deletes a conversation note this many days + 1 after its session date, and empties it to a tombstone the same interval later.'
WHERE key = 'conversation_retention_days';
