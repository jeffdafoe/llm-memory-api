-- MEM-139: sim_actor_id / sim_actor_name columns on virtual_agent_calls.
--
-- Attribution for shared switchboard-VA turns (LLM-236). The salem engine backs
-- MANY in-world actors (Anne / Patience / Silence Walker ...) with ONE shared VA
-- agent (salem-vendor / salem-visitor), so virtual_agent_calls.actor_id — which
-- is the VA agent, not the character — collapses every character onto one
-- row-owner. Which actor a turn was FOR was only recoverable by parsing the
-- perception text in user_message, which made huddle forensics slow (the
-- motivating case: a 122-turn Inn huddle where 66 turns were salem-vendor across
-- three interleaved actors).
--
-- The engine already knows the deliberating actor, so it now stamps the actor's
-- id + display name on the /v1/chat/send body; memory_api stores them here and
-- surfaces them on /v1/sim/raw-turns (which the salem umbilical's /turns proxy
-- relays). Persistent 1:1 VAs (zbbs-<name>) stamp it too, so filtering by
-- in-world actor is uniform across shared and stateful NPCs.
--
-- sim_actor_id is TEXT, not uuid: it is the salem ENGINE actor id (from its own
-- zbbs database), opaque to memory_api and never joined here — TEXT sidesteps the
-- uuid-cast 500 an ill-formed value would otherwise raise (the same reason
-- conversation_id is TEXT, MEM-133). sim_actor_name mirrors scene_structure's
-- VARCHAR(100) (zbbs display names are bounded there). Both NULL for
-- companion-mode / human chat and for village-level cascades (atmosphere /
-- noticeboard / narration expansion) that act on behalf of no single actor.
--
-- The partial index mirrors idx_va_calls_scene: a btree over the non-null rows
-- for the /sim/raw-turns sim_actor filter (huddle forensics: "every turn this
-- character took").

ALTER TABLE virtual_agent_calls ADD COLUMN sim_actor_id TEXT;
ALTER TABLE virtual_agent_calls ADD COLUMN sim_actor_name VARCHAR(100);
CREATE INDEX idx_va_calls_sim_actor ON virtual_agent_calls (sim_actor_id) WHERE sim_actor_id IS NOT NULL;
