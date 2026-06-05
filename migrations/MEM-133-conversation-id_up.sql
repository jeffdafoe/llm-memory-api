-- MEM-133: conversation_id column on chat_message_texts + virtual_agent_calls.
--
-- The admin comms chat viewer groups chat rows by scene_id, but the salem engine
-- mints a fresh scene_id PER TICK (llm.NewSceneID), so one conversation shatters
-- into one collapsible group per tick. conversation_id carries the engine's
-- narrative-beat scene (sim.Scene.ID) instead — STABLE across the ticks AND the
-- participants of one conversation beat — so the viewer can collapse the whole
-- exchange back into one conversation (with the per-tick scene_id as the inner
-- sub-group). Threaded from salem via the /v1/chat/send body (ZBBS-HOME-397).
--
-- TEXT, not uuid (unlike scene_id): sim.SceneID is "sc-"+hex, not a UUID.
-- Nullable — NULL for companion-mode, human chat, and solo (no-huddle) sim ticks,
-- which then render ungrouped exactly like a NULL scene_id does today.
--
-- Mirrors idx_cmt_scene / idx_va_calls_scene: a partial btree index over the
-- non-null rows for the viewer's group-by and the call-log correlation.

ALTER TABLE chat_message_texts ADD COLUMN conversation_id TEXT;
CREATE INDEX idx_cmt_conversation ON chat_message_texts (conversation_id) WHERE conversation_id IS NOT NULL;

ALTER TABLE virtual_agent_calls ADD COLUMN conversation_id TEXT;
CREATE INDEX idx_va_calls_conversation ON virtual_agent_calls (conversation_id) WHERE conversation_id IS NOT NULL;
