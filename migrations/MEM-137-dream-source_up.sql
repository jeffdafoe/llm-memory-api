-- MEM-137 — per-agent dream source: conversation logs (default) or curated
-- notes (ZBBS-WORK-391).
--
-- Motivating case: a companion-mode account whose memory model is hand
-- curation — it has zero conversations/* notes, so dream_mode=companion is a
-- silent no-op (the dream cron sources exclusively from conversations/%).
-- Its substance lives in curated notes (identity docs, journals, session
-- summaries) keyed by updated_at.
--
-- dream_source selects where the cron reads its raw material:
--   'conversation' — conversations/% windowed by created_at (existing
--                    behavior, the default; nothing changes for existing
--                    agents).
--   'notes'        — all namespace notes EXCEPT the pipeline's own outputs
--                    (conversations/dreams/context/learnings prefixes),
--                    windowed by updated_at. An edit to a curated note
--                    re-enters it as fresh dream material on the next run.
--
-- TEXT + CHECK rather than an enum type: two values, and a CHECK is cheaper
-- to extend or drop than ALTER TYPE ... ADD VALUE (which can't run inside a
-- transaction on older PG). dream_mode predates this convention.

ALTER TABLE agent_configuration
    ADD COLUMN dream_source TEXT NOT NULL DEFAULT 'conversation'
    CONSTRAINT agent_configuration_dream_source_check
    CHECK (dream_source IN ('conversation', 'notes'));
