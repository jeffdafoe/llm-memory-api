-- MEM-124: One-shot cleanup of conversations/* notes for agents with
-- dream_mode='none'.
--
-- The conversations/* note path exists primarily to feed the dream
-- pipeline at night — dream-companion / dream-technical / dream-sim
-- each consume the per-call transcripts written by logTranscript and
-- consolidate them into dreams/* notes. Agents with dream_mode='none'
-- (overseers like salem-chronicler, utility VAs like code_review /
-- search-general / memory-enrichment) have no dream consumer; their
-- conversations/* notes were stored, chunked, embedded, and indexed
-- but read by nothing, and showed up as noise in cross-namespace
-- semantic search.
--
-- Going forward, logTranscript skips writing for dream_mode='none'
-- agents. This migration is the one-shot historical cleanup. The
-- structured audit trail in virtual_agent_calls (logCall) is unaffected
-- — that's where call_detail / debug visibility lives.
--
-- Three steps, in order:
--   1. Hard-delete vector chunks for the matching notes (so they stop
--      surfacing in search the moment this lands, not "after the next
--      decay sweep").
--   2. Soft-delete the documents (deleted_at = NOW()) — preserves
--      content for restore-from-trash if anyone realizes they wanted
--      the note back, mirroring the deleteNote() service path.
--   3. Recompute namespace_usage for the affected namespaces from
--      scratch — simpler and less error-prone than tallying deltas.
--
-- Down migration is intentionally a no-op for the same reason MEM-123's
-- was: un-soft-deleting these would mix the migration's actions with
-- legitimate later soft-deletes (decay, admin), and the chunks are
-- already gone — restoring would require re-chunking, which the
-- restoreNote() service path does explicitly via re-save, not via SQL.

-- 1. Vector chunks
DELETE FROM memory_chunks mc
USING documents d, actors a, agent_configuration agc
WHERE mc.namespace = d.namespace
  AND LOWER(mc.source_file) = LOWER(d.slug)
  AND d.namespace = a.name
  AND a.id = agc.actor_id
  AND d.slug LIKE 'conversations/%'
  AND d.deleted_at IS NULL
  AND agc.dream_mode = 'none';

-- 2. Soft-delete documents
UPDATE documents d
SET deleted_at = NOW()
FROM actors a, agent_configuration agc
WHERE d.namespace = a.name
  AND a.id = agc.actor_id
  AND d.slug LIKE 'conversations/%'
  AND d.deleted_at IS NULL
  AND agc.dream_mode = 'none';

-- 3. Recompute namespace_usage for affected namespaces
UPDATE namespace_usage nu
SET note_count = (
        SELECT COUNT(*)
        FROM documents d
        WHERE d.namespace = nu.namespace AND d.deleted_at IS NULL
    ),
    total_bytes = (
        SELECT COALESCE(SUM(LENGTH(content)), 0)
        FROM documents d
        WHERE d.namespace = nu.namespace AND d.deleted_at IS NULL
    ),
    updated_at = NOW()
WHERE nu.namespace IN (
    SELECT a.name FROM actors a
    JOIN agent_configuration agc ON agc.actor_id = a.id
    WHERE agc.dream_mode = 'none'
);
