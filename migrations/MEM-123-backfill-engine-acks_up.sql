-- MEM-123: Backfill auto-ack for replies addressed to salem-engine.
--
-- The salem-engine Go service consumes its inbound chat replies inline via
-- the wait=true response on /v1/chat/send — it never separately reads
-- chat_messages to fetch them and never calls /chat/ack. Result: every
-- chat_messages delivery row addressed to salem-engine sat with
-- acked_at IS NULL forever, leaving every Salem scene visually unacked
-- in the admin chat list (the scene-row hasUnacked indicator was always
-- lit, regardless of whether anything was actually pending).
--
-- Going forward this is enforced by the chatSend ackOnInsert path, set
-- by handleDirectChat for replies whose original /chat/send was wait=true.
-- This migration is the one-shot historical cleanup; without it the
-- backlog of unacked engine-bound rows stays unacked even after the
-- code fix lands.
--
-- We stamp acked_at = sent_at (not NOW()) so the historical timeline
-- reflects when the engine actually consumed each reply, not when this
-- migration ran.
--
-- Predicate is scoped tightly to the wait=true inline engine reply
-- pattern: rows addressed to salem-engine, from a *different* actor,
-- where the sender is a virtual agent (NPCs and the chronicler). That
-- excludes admin/human-initiated messages, system messages, and any
-- non-VA sender — those rows might legitimately need user-driven ack
-- semantics that this backfill should not paper over.

UPDATE chat_messages cm
SET acked_at = cmt.sent_at
FROM chat_message_texts cmt
JOIN actors to_a ON to_a.name = 'salem-engine'
JOIN actors from_a ON from_a.id = cmt.from_actor_id
JOIN agent_configuration from_agc ON from_agc.actor_id = from_a.id
WHERE cm.message_text_id = cmt.id
  AND cm.to_actor_id = to_a.id
  AND cm.acked_at IS NULL
  AND cm.deleted_at IS NULL
  AND from_a.name <> 'salem-engine'
  AND from_agc.virtual = TRUE;
