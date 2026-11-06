//go:build contract

package contract

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The fixture's virtual agent: created by the admin, with no
// virtual_agent_access rows, so agent-a and agent-b may not use it and mail
// to it gets the unanswerable notice back. The ones agent-a may use are in
// fixture_va.go.
const VirtualAgentName = "helper-va"

// Fixed mail ids, so cases can name fixture messages.
const (
	mailUnreadToA   = "6d2a0c1e-0000-4000-8000-0000000000a1" // agent-b → agent-a, unread
	mailReadByA     = "6d2a0c1e-0000-4000-8000-0000000000a2" // agent-b → agent-a, acked
	mailUnreadToB   = "6d2a0c1e-0000-4000-8000-0000000000b1" // agent-a → agent-b, unread, a reply
	mailUnsentToB   = "6d2a0c1e-0000-4000-8000-0000000000b2" // agent-a → agent-b, unsent
	mailReadByB     = "6d2a0c1e-0000-4000-8000-0000000000b3" // agent-a → agent-b, acked, long body
	mailFromVAToA   = "6d2a0c1e-0000-4000-8000-0000000000c1" // helper-va → agent-a, unread
	mailLongBodyLen = 260
)

// applyMailFixture adds the virtual agent and mail between the agents with
// fixed times, so check, sent and history have rows of every state to list.
func applyMailFixture(ctx context.Context, tx pgx.Tx, adminID int, agents map[string]int) error {
	var vaID int
	if err := tx.QueryRow(ctx, "INSERT INTO actors (name, created_by) VALUES ($1, $2) RETURNING id", VirtualAgentName, adminID).Scan(&vaID); err != nil {
		return fmt.Errorf("virtual agent: %w", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO agent_configuration (actor_id, virtual) VALUES ($1, true)", vaID); err != nil {
		return err
	}

	a, b := agents[AgentAName], agents[AgentBName]
	long := ""
	for len(long) < mailLongBodyLen {
		long += "Long body line, past the 200-character preview. "
	}
	rows := []struct {
		id, subject, body  string
		from, to           int
		sentAt             string
		ackedAt, deletedAt *string
		inReplyTo          *string
	}{
		{id: mailUnreadToA, subject: "Fixture unread", body: "Unread body for agent-a.", from: b, to: a, sentAt: "2026-10-01 10:00:00+00"},
		{id: mailReadByA, subject: "Fixture read", body: "Read body for agent-a.", from: b, to: a, sentAt: "2026-10-02 10:00:00+00", ackedAt: ptr("2026-10-02 11:00:00+00")},
		{id: mailUnreadToB, subject: "Re: Fixture unread", body: "Reply to agent-b.", from: a, to: b, sentAt: "2026-10-03 10:00:00+00", inReplyTo: ptr(mailUnreadToA)},
		{id: mailUnsentToB, subject: "Fixture unsent", body: "Taken back.", from: a, to: b, sentAt: "2026-10-04 10:00:00+00", deletedAt: ptr("2026-10-04 10:05:00.123456+00")},
		{id: mailReadByB, subject: "Fixture long", body: long, from: a, to: b, sentAt: "2026-10-05 10:00:00.654321+00", ackedAt: ptr("2026-10-05 12:00:00+00"), inReplyTo: ptr(mailReadByA)},
		{id: mailFromVAToA, subject: "From the helper", body: "Hello from a virtual agent.", from: vaID, to: a, sentAt: "2026-10-06 10:00:00+00"},
	}
	for _, r := range rows {
		if _, err := tx.Exec(ctx,
			`INSERT INTO mail (id, from_actor_id, to_actor_id, subject, body, sent_at, acked_at, deleted_at, in_reply_to)
             VALUES ($1, $2, $3, $4, $5, $6::timestamptz, $7::timestamptz, $8::timestamptz, $9::uuid)`,
			r.id, r.from, r.to, r.subject, r.body, r.sentAt, r.ackedAt, r.deletedAt, r.inReplyTo); err != nil {
			return fmt.Errorf("mail %s: %w", r.id, err)
		}
	}
	return nil
}

func ptr(s string) *string { return &s }
