//go:build contract

package contract

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The fixture's discussion: active, with agent-a and agent-b joined and
// helper-va only invited. Node's discussion trigger loads it, finds no
// joined virtual agent, and stops before any write or provider call.
const fixtureDiscussionID = 101

// applyChatFixture adds the discussion and chat messages with fixed ids and
// times, so receive and status have rows of every state to list. The id
// sequences start past the fixed ids.
func applyChatFixture(ctx context.Context, tx pgx.Tx, adminID int, agents map[string]int) error {
	a, b := agents[AgentAName], agents[AgentBName]
	var vaID int
	if err := tx.QueryRow(ctx, "SELECT id FROM actors WHERE name = $1", VirtualAgentName).Scan(&vaID); err != nil {
		return fmt.Errorf("virtual agent: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO discussions (id, topic, status, created_by_actor_id, created_at)
         VALUES ($1, 'Contract discussion', 'active', $2, '2026-10-01 09:00:00+00')`, fixtureDiscussionID, adminID); err != nil {
		return fmt.Errorf("discussion: %w", err)
	}
	for _, p := range []struct {
		actor  int
		status string
	}{{a, "joined"}, {b, "joined"}, {vaID, "invited"}} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO discussion_participants (discussion_id, actor_id, status, invited_at)
             VALUES ($1, $2, $3, '2026-10-01 09:00:00+00')`, fixtureDiscussionID, p.actor, p.status); err != nil {
			return fmt.Errorf("participant: %w", err)
		}
	}

	texts := []struct {
		id                         int
		message                    string
		from                       int
		discussionID               *int
		sentAt                     string
		toolCalls                  *string
		toolCallID, sceneID, scene *string
		conversationID             *string
	}{
		{id: 1, message: "Unread direct message.", from: b, sentAt: "2026-10-01 10:00:00+00"},
		{id: 2, message: "Read direct message.", from: b, sentAt: "2026-10-02 10:00:00.654321+00"},
		{id: 3, message: "In the discussion.", from: b, discussionID: intPtr(fixtureDiscussionID), sentAt: "2026-10-03 10:00:00+00"},
		{id: 4, message: "A tool turn.", from: vaID, sentAt: "2026-10-04 10:00:00+00",
			toolCalls: ptr(`[{"id": "c1", "type": "function", "function": {"name": "speak", "arguments": "{}"}}]`),
			sceneID:   ptr(fixtureSceneID), scene: ptr("Tavern"), conversationID: ptr("sc-1")},
		{id: 5, message: "To agent-b.", from: a, sentAt: "2026-10-05 10:00:00+00"},
		{id: 6, message: "Taken back.", from: a, sentAt: "2026-10-06 10:00:00+00"},
	}
	for _, t := range texts {
		if _, err := tx.Exec(ctx,
			`INSERT INTO chat_message_texts (id, message, from_actor_id, discussion_id, sent_at, tool_calls, tool_call_id, scene_id, scene_structure, conversation_id)
             VALUES ($1, $2, $3, $4, $5::timestamptz, $6::jsonb, $7, $8::uuid, $9, $10)`,
			t.id, t.message, t.from, t.discussionID, t.sentAt, t.toolCalls, t.toolCallID, t.sceneID, t.scene, t.conversationID); err != nil {
			return fmt.Errorf("chat text %d: %w", t.id, err)
		}
	}
	deliveries := []struct {
		id, textID, to     int
		ackedAt, deletedAt *string
	}{
		{id: 1, textID: 1, to: a},
		{id: 2, textID: 2, to: a, ackedAt: ptr("2026-10-02 11:00:00+00")},
		{id: 3, textID: 3, to: a},
		{id: 4, textID: 4, to: a},
		{id: 5, textID: 5, to: b},
		{id: 6, textID: 6, to: b, deletedAt: ptr("2026-10-06 10:05:00+00")},
	}
	for _, d := range deliveries {
		if _, err := tx.Exec(ctx,
			`INSERT INTO chat_messages (id, message_text_id, to_actor_id, acked_at, deleted_at)
             VALUES ($1, $2, $3, $4::timestamptz, $5::timestamptz)`,
			d.id, d.textID, d.to, d.ackedAt, d.deletedAt); err != nil {
			return fmt.Errorf("chat delivery %d: %w", d.id, err)
		}
	}
	if _, err := tx.Exec(ctx, `SELECT setval('chat_message_texts_id_seq', 100), setval('chat_messages_id_seq', 100)`); err != nil {
		return err
	}
	return nil
}

func intPtr(n int) *int { return &n }
