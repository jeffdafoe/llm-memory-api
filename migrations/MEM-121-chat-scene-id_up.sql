-- MEM-121: Scene grouping for sim-mode chat (Salem M6.4 follow-up).
--
-- A "scene" is one cascade of related ticks: a player or NPC speaks, all
-- co-located NPCs react, their replies trigger more reactions, and so on
-- until the chain quiets. The salem-engine mints one UUID at the cascade
-- origin (PC speak, NPC arrival, baseline hourly tick) and threads it
-- through `triggerImmediateTick` / `triggerCoLocatedTicks` /
-- `executeAgentCommit`, so every chat row and provider call produced by
-- that cascade carries the same scene_id.
--
-- Walks intentionally do not carry scene_id forward: a `move_to`
-- finishes seconds-to-minutes of game time later, by which point the
-- arrival is a new scene. The engine mints a fresh UUID at the arrival
-- trigger.
--
-- Companion-mode chat rows leave scene_id NULL — those aren't scenes,
-- and the admin UI renders them as today.
--
-- Two columns, one each on chat_message_texts and virtual_agent_calls:
-- the chat row is the message itself; the call row is the LLM provider
-- exchange that produced (or consumed) it. Both are useful filters in
-- the admin UI — chat shows what was said, calls show what was sent
-- to the model.
--
-- Partial indexes only — the vast majority of historical and
-- companion-mode rows will have NULL scene_id, and we only ever query
-- by exact scene_id match.

ALTER TABLE chat_message_texts ADD COLUMN scene_id UUID;
CREATE INDEX idx_cmt_scene ON chat_message_texts (scene_id) WHERE scene_id IS NOT NULL;

ALTER TABLE virtual_agent_calls ADD COLUMN scene_id UUID;
CREATE INDEX idx_va_calls_scene ON virtual_agent_calls (scene_id) WHERE scene_id IS NOT NULL;
