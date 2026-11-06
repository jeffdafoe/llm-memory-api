//go:build contract

package contract

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applyDiscussionFixture adds the discussions, votes and ballots the
// discussion cases name (cases_discussion.go, where their ids are).
func applyDiscussionFixture(ctx context.Context, tx pgx.Tx, adminID int, agents map[string]int) error {
	a, b := agents[AgentAName], agents[AgentBName]
	var vaID int
	if err := tx.QueryRow(ctx, "SELECT id FROM actors WHERE name = $1", VirtualAgentName).Scan(&vaID); err != nil {
		return fmt.Errorf("virtual agent: %w", err)
	}

	type participant struct {
		actor        int
		status, role string
		joinedAt     *string
		deferredAt   *string
		deferCount   *int
	}
	discussions := []struct {
		id                   int
		topic, status, mode  string
		creator              int
		createdAt            string
		timeoutAt            *string
		concludedAt, outcome *string
		channel, context     *string
		participants         []participant
	}{
		{id: 102, topic: "Waiting on agent-b", status: "waiting", mode: "realtime", creator: a,
			createdAt: "2026-10-02 09:00:00+00", timeoutAt: ptr("2099-01-01 00:00:00+00"), channel: ptr("waiting-room"),
			participants: []participant{
				{actor: a, status: "joined", role: "required", joinedAt: ptr("2026-10-02 09:00:00+00")},
				{actor: b, status: "invited", role: "required", deferCount: intPtr(0)},
			}},
		{id: 103, topic: "Optional member never came", status: "waiting", mode: "realtime", creator: a,
			createdAt: "2026-10-03 09:00:00+00", timeoutAt: ptr("2026-10-03 09:05:00+00"),
			participants: []participant{
				{actor: a, status: "joined", role: "required", joinedAt: ptr("2026-10-03 09:00:00+00")},
				{actor: b, status: "joined", role: "required", joinedAt: ptr("2026-10-03 09:01:00+00")},
				{actor: vaID, status: "invited", role: "optional"},
			}},
		{id: 104, topic: "Already settled", status: "concluded", mode: "async", creator: b,
			createdAt: "2026-10-04 09:00:00+00", concludedAt: ptr("2026-10-04 12:00:00+00"), outcome: ptr("consensus"),
			context: ptr("Background for the settled topic."),
			participants: []participant{
				{actor: a, status: "left", role: "required", joinedAt: ptr("2026-10-04 09:10:00+00")},
				{actor: b, status: "left", role: "required", joinedAt: ptr("2026-10-04 09:00:00+00")},
			}},
		{id: 105, topic: "Async votes", status: "active", mode: "async", creator: a,
			createdAt: "2026-10-05 09:00:00+00", timeoutAt: ptr("2026-10-06 09:00:00+00"),
			participants: []participant{
				{actor: a, status: "joined", role: "required", joinedAt: ptr("2026-10-05 09:00:00+00")},
				{actor: b, status: "joined", role: "required", joinedAt: ptr("2026-10-05 09:30:00+00"), deferredAt: ptr("2026-10-05 09:10:00+00"), deferCount: intPtr(1)},
			}},
		{id: 109, topic: "Nobody answered", status: "waiting", mode: "async", creator: a,
			createdAt: "2026-10-09 09:00:00+00", timeoutAt: ptr("2026-10-10 09:00:00+00"),
			participants: []participant{
				{actor: a, status: "joined", role: "required", joinedAt: ptr("2026-10-09 09:00:00+00")},
				{actor: b, status: "invited", role: "required", deferredAt: ptr("2026-10-09 10:00:00+00"), deferCount: intPtr(3)},
			}},
	}
	for _, d := range discussions {
		if _, err := tx.Exec(ctx,
			`INSERT INTO discussions (id, topic, status, channel, created_at, concluded_at, mode, context, timeout_at, outcome, created_by_actor_id)
             VALUES ($1, $2, $3, $4, $5::timestamptz, $6::timestamptz, $7, $8, $9::timestamptz, $10, $11)`,
			d.id, d.topic, d.status, d.channel, d.createdAt, d.concludedAt, d.mode, d.context, d.timeoutAt, d.outcome, d.creator); err != nil {
			return fmt.Errorf("discussion %d: %w", d.id, err)
		}
		for _, p := range d.participants {
			if _, err := tx.Exec(ctx,
				`INSERT INTO discussion_participants (discussion_id, actor_id, status, role, invited_at, joined_at, deferred_at, defer_count)
                 VALUES ($1, $2, $3, $4, $5::timestamptz, $6::timestamptz, $7::timestamptz, $8)`,
				d.id, p.actor, p.status, p.role, d.createdAt, p.joinedAt, p.deferredAt, p.deferCount); err != nil {
				return fmt.Errorf("participant of %d: %w", d.id, err)
			}
		}
	}

	votes := []struct {
		id, discussion                   int
		question, status, typ, threshold string
		proposer                         int
		closesAt, createdAt              string
		closedAt                         *string
	}{
		{201, 105, "Adopt plan A? 1=yes 2=no", "open", "general", "unanimous", a, "2099-01-01 00:00:00+00", "2026-10-05 10:00:00+00", nil},
		{202, 105, "Conclude? 1=yes 2=no", "open", "conclude", "majority", b, "2099-01-01 00:00:00+00", "2026-10-05 11:00:00+00", nil},
		{203, 105, "Adopt plan B? 1=yes 2=no", "closed", "general", "unanimous", a, "2099-01-01 00:00:00+00", "2026-10-05 09:40:00+00", ptr("2026-10-05 09:50:00+00")},
		{204, 105, "Adopt plan C? 1=yes 2=no", "open", "general", "majority", a, "2026-10-05 12:30:00+00", "2026-10-05 12:00:00+00", nil},
		{205, 101, "Realtime question? 1=yes 2=no", "open", "general", "unanimous", a, "2099-01-01 00:00:00+00", "2026-10-01 09:30:00+00", nil},
	}
	for _, v := range votes {
		if _, err := tx.Exec(ctx,
			`INSERT INTO discussion_votes (id, discussion_id, question, status, type, threshold, closes_at, created_at, closed_at, proposed_by_actor_id)
             VALUES ($1, $2, $3, $4, $5, $6, $7::timestamptz, $8::timestamptz, $9::timestamptz, $10)`,
			v.id, v.discussion, v.question, v.status, v.typ, v.threshold, v.closesAt, v.createdAt, v.closedAt, v.proposer); err != nil {
			return fmt.Errorf("vote %d: %w", v.id, err)
		}
	}
	ballots := []struct {
		vote, actor, choice int
		reason              *string
		castAt              string
	}{
		{201, a, 1, ptr("Plan A is sound."), "2026-10-05 10:10:00+00"},
		{203, a, 1, nil, "2026-10-05 09:41:00+00"},
		{203, b, 2, ptr("Plan B costs too much."), "2026-10-05 09:42:00+00"},
	}
	for _, bl := range ballots {
		if _, err := tx.Exec(ctx,
			`INSERT INTO discussion_ballots (vote_id, actor_id, choice, reason, cast_at) VALUES ($1, $2, $3, $4, $5::timestamptz)`,
			bl.vote, bl.actor, bl.choice, bl.reason, bl.castAt); err != nil {
			return fmt.Errorf("ballot of %d: %w", bl.vote, err)
		}
	}
	_, err := tx.Exec(ctx, `SELECT setval('discussions_id_seq', 200), setval('discussion_votes_id_seq', 300)`)
	return err
}
