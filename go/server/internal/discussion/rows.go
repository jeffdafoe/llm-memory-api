package discussion

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// Timestamp is a timestamptz column. JSON writes it as node-postgres's Date
// reached JSON.stringify; callers that printed the Date in a template (the
// MCP tools) have the time itself.
type Timestamp struct {
	time.Time
}

// MarshalJSON writes the time as Date.toISOString.
func (t Timestamp) MarshalJSON() ([]byte, error) {
	return []byte(`"` + jsutil.DateISO(t.Time) + `"`), nil
}

func optionalTime(t *time.Time) *Timestamp {
	if t == nil {
		return nil
	}
	return &Timestamp{*t}
}

// The column lists are the tables' column order, which is what Node's
// `d.*` and `v.*` returned, so the response keys come out in the same order.
const (
	discussionColumns = "d.id, d.topic, d.status, d.channel, d.created_at, d.concluded_at, d.mode, d.context, d.timeout_at, d.outcome, d.created_by_actor_id"
	voteColumns       = "v.id, v.discussion_id, v.question, v.status, v.type, v.threshold, v.closes_at, v.created_at, v.closed_at, v.proposed_by_actor_id"
)

// DiscussionRow is a discussions row (`d.*`).
type DiscussionRow struct {
	ID               int        `json:"id"`
	Topic            string     `json:"topic"`
	Status           string     `json:"status"`
	Channel          *string    `json:"channel"`
	CreatedAt        Timestamp  `json:"created_at"`
	ConcludedAt      *Timestamp `json:"concluded_at"`
	Mode             string     `json:"mode"`
	Context          *string    `json:"context"`
	TimeoutAt        *Timestamp `json:"timeout_at"`
	Outcome          *string    `json:"outcome"`
	CreatedByActorID int        `json:"created_by_actor_id"`
}

// Discussion is a discussions row with its creator's name (`d.*, ac.name AS
// created_by`); nil is null.
type Discussion struct {
	DiscussionRow
	CreatedBy *string `json:"created_by"`
}

func scanDiscussionRow(row pgx.Row, extra ...any) (DiscussionRow, error) {
	var d DiscussionRow
	var concludedAt, timeoutAt *time.Time
	dest := append([]any{&d.ID, &d.Topic, &d.Status, &d.Channel, &d.CreatedAt.Time, &concludedAt, &d.Mode, &d.Context, &timeoutAt, &d.Outcome, &d.CreatedByActorID}, extra...)
	if err := row.Scan(dest...); err != nil {
		return d, err
	}
	d.ConcludedAt = optionalTime(concludedAt)
	d.TimeoutAt = optionalTime(timeoutAt)
	return d, nil
}

// queryDiscussions runs a query selecting discussionColumns and the creator
// name.
func queryDiscussions(ctx context.Context, conn db.DB, sql string, args ...any) ([]Discussion, error) {
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Discussion, error) {
		var name string
		d, err := scanDiscussionRow(row, &name)
		return Discussion{DiscussionRow: d, CreatedBy: &name}, err
	})
}

// VoteRow is a discussion_votes row (`v.*`).
type VoteRow struct {
	ID                int        `json:"id"`
	DiscussionID      int        `json:"discussion_id"`
	Question          string     `json:"question"`
	Status            string     `json:"status"`
	Type              string     `json:"type"`
	Threshold         string     `json:"threshold"`
	ClosesAt          *Timestamp `json:"closes_at"`
	CreatedAt         Timestamp  `json:"created_at"`
	ClosedAt          *Timestamp `json:"closed_at"`
	ProposedByActorID int        `json:"proposed_by_actor_id"`
}

// Vote is a vote with its proposer's name (`v.*, ac.name AS proposed_by`).
type Vote struct {
	VoteRow
	ProposedBy string `json:"proposed_by"`
}

// PendingVote is an open vote in the pending route's list, with its
// discussion's topic and mode.
type PendingVote struct {
	Vote
	DiscussionTopic string `json:"discussion_topic"`
	DiscussionMode  string `json:"discussion_mode"`
}

func scanVoteRow(row pgx.Row, extra ...any) (VoteRow, error) {
	var v VoteRow
	var closesAt, closedAt *time.Time
	dest := append([]any{&v.ID, &v.DiscussionID, &v.Question, &v.Status, &v.Type, &v.Threshold, &closesAt, &v.CreatedAt.Time, &closedAt, &v.ProposedByActorID}, extra...)
	if err := row.Scan(dest...); err != nil {
		return v, err
	}
	v.ClosesAt = optionalTime(closesAt)
	v.ClosedAt = optionalTime(closedAt)
	return v, nil
}

func queryVotes(ctx context.Context, conn db.DB, sql string, args ...any) ([]Vote, error) {
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Vote, error) {
		var name string
		v, err := scanVoteRow(row, &name)
		return Vote{VoteRow: v, ProposedBy: name}, err
	})
}

// Participant is one row of a discussion's participant list.
type Participant struct {
	Agent      string     `json:"agent"`
	Status     string     `json:"status"`
	Role       string     `json:"role"`
	InvitedAt  Timestamp  `json:"invited_at"`
	JoinedAt   *Timestamp `json:"joined_at"`
	DeferredAt *Timestamp `json:"deferred_at"`
	DeferCount *int       `json:"defer_count"`
}

const participantsOf = `SELECT ac.name AS agent, dp.status, dp.role, dp.invited_at, dp.joined_at, dp.deferred_at, dp.defer_count
         FROM discussion_participants dp
         JOIN actors ac ON ac.id = dp.actor_id
         WHERE dp.discussion_id = $1`

func queryParticipants(ctx context.Context, conn db.DB, discussionID any) ([]Participant, error) {
	rows, err := conn.Query(ctx, participantsOf, discussionID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Participant, error) {
		var p Participant
		var joinedAt, deferredAt *time.Time
		err := row.Scan(&p.Agent, &p.Status, &p.Role, &p.InvitedAt.Time, &joinedAt, &deferredAt, &p.DeferCount)
		p.JoinedAt, p.DeferredAt = optionalTime(joinedAt), optionalTime(deferredAt)
		return p, err
	})
}

// Ballot is one cast ballot of a vote.
type Ballot struct {
	Agent  string    `json:"agent"`
	Choice int       `json:"choice"`
	Reason *string   `json:"reason"`
	CastAt Timestamp `json:"cast_at"`
}

const ballotsOf = `SELECT ac.name AS agent, b.choice, b.reason, b.cast_at
         FROM discussion_ballots b
         JOIN actors ac ON ac.id = b.actor_id
         WHERE b.vote_id = $1 ORDER BY b.cast_at ASC`

func queryBallots(ctx context.Context, conn db.DB, voteID any) ([]Ballot, error) {
	rows, err := conn.Query(ctx, ballotsOf, voteID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Ballot, error) {
		var b Ballot
		err := row.Scan(&b.Agent, &b.Choice, &b.Reason, &b.CastAt.Time)
		return b, err
	})
}

// choiceCount is a row of `SELECT choice, COUNT(*) ... GROUP BY choice`.
type choiceCount struct {
	choice int
	count  int64
}

func ballotCounts(ctx context.Context, conn db.DB, voteID any) ([]choiceCount, error) {
	rows, err := conn.Query(ctx, "SELECT choice, COUNT(*) as count FROM discussion_ballots WHERE vote_id = $1 GROUP BY choice", voteID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (choiceCount, error) {
		var c choiceCount
		err := row.Scan(&c.choice, &c.count)
		return c, err
	})
}
