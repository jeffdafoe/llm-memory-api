-- MEM-142 — per-actor dream roster for shared-VA NPCs; enroll salem-vendor as
-- 'sim-shared' (LLM-515, Slice 1).
--
-- A shared-VA NPC is a slug-prefix (e.g. 'constance-scott/') inside a pooled
-- agent's note namespace, not its own actors row — so it can't be modeled as an
-- agent_configuration row. This roster is the lightweight per-actor dream record
-- instead: one row per (pooled agent, actor slug-prefix), populated by the daily
-- conversation-day push (sim-conversation-distiller.js) and consumed by the
-- per-actor dream loop (Slice 2, dream.js).
--
--   shared_actor_id — the POOLED agent's actors.id (salem-vendor's), FK.
--   slug_prefix     — the villager's memory partition prefix, e.g.
--                     'constance-scott/'. Together with shared_actor_id it keys
--                     the row.
--   display_name    — the villager's display name, for the distilled note's line
--                     labels and (Slice 2) the dream material.
--   last_pushed_day — YYYY-MM-DD of the most recent conversation-day the push
--                     landed for this actor; upserted by the push.
--   last_dream_at   — when Slice 2's dream loop last consolidated this actor
--                     (NULL until then).

BEGIN;

CREATE TABLE sim_shared_actor (
    shared_actor_id INTEGER     NOT NULL REFERENCES actors(id) ON DELETE CASCADE,
    slug_prefix     TEXT        NOT NULL,
    display_name    TEXT        NOT NULL,
    last_pushed_day TEXT,
    last_dream_at   TIMESTAMPTZ,
    PRIMARY KEY (shared_actor_id, slug_prefix)
);

-- Enroll the pooled vendor agent. salem-visitor stays 'none' for now — its NPCs
-- are transient one-off merchants (LLM-455), so persistent per-actor memory would
-- accrue dead notes with no prune path. salem-generic stays 'none' (throwaway
-- generics). Both can be enrolled later by setting dream_mode = 'sim-shared'.
UPDATE agent_configuration
   SET dream_mode = 'sim-shared'
 WHERE actor_id IN (SELECT id FROM actors WHERE name = 'salem-vendor');

COMMIT;
