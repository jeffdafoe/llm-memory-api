-- MEM-129: backfill scene_structure on historical chat_message_texts
-- rows from a sibling row in the same scene that DID carry one.
--
-- MEM-127 added the scene_structure column. The engine populates it
-- on the perception build of a scene head, but follow-up tool-call
-- and tool-result rows in the same scene typically don't carry it.
-- The admin chat UI groups by scene_id and labels each scene with
-- the FIRST row's scene_structure — when that row is a tool-result
-- (no structure), the scene renders as bare uuid.
--
-- MEM-128 (commit f7e6629) closed the going-forward path via
-- COALESCE-with-subquery on insert. This migration backfills
-- historical rows: for each scene_id that has at least one
-- non-null scene_structure, propagate that value (the EARLIEST
-- non-null row in id order) onto every NULL row in the same scene.
--
-- Rows with no labeled sibling stay NULL — those are scenes where
-- nothing carried scene_structure ever (pre-MEM-127 history, or
-- engine paths that don't pass it). The companion engine work in
-- the salem-engine repo (force-tick perception inserts +
-- agent-reply tool_call inserts) closes those gaps going forward;
-- the historical records of those paths can't be recovered.
--
-- Idempotent: re-running this migration after the data is
-- backfilled is a no-op (the join finds matching pairs but every
-- target row's scene_structure is already non-null, so the
-- WHERE filter excludes them).
--
-- Estimated affected rows: ~377 (queried from production
-- 2026-05-07 ~15:20 UTC).

BEGIN;

UPDATE chat_message_texts cmt
   SET scene_structure = src.scene_structure
  FROM (
    SELECT DISTINCT ON (scene_id) scene_id, scene_structure
      FROM chat_message_texts
     WHERE scene_id IS NOT NULL
       AND scene_structure IS NOT NULL
     ORDER BY scene_id, id ASC
  ) src
 WHERE cmt.scene_id = src.scene_id
   AND cmt.scene_structure IS NULL;

COMMIT;
