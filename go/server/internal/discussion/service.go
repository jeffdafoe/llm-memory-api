// Package discussion is Node's services/discussion.js and
// routes/discussion.js: multi-agent discussions, their participants, and
// votes on them. The service functions take names and ids, not requests,
// because the MCP tools, the admin routes and the virtual agents call them
// too. The REST routes carry their own copies of most steps, as in Node, and
// differ from the service in places (routes.go).
package discussion

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgbind"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/system"
)

// Service is the discussion service.
type Service struct {
	DB       db.DB
	Config   *config.Store
	Actors   *actors.Cache
	Log      *logger.Logger
	Notifier *system.Notifier
	// Now is the clock JavaScript's Date read; nil is time.Now.
	Now func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log(action string, details logger.Fields) {
	s.Log.Log("discussion", action, details)
}

func field(key string, value any) logger.Field {
	return logger.Field{Key: key, Value: value}
}

// orderKey marks a context whose fire-and-forget jobs run one after the
// other, in the order they were started.
type orderKey struct{}

type jobOrder struct {
	mu   sync.Mutex
	last chan struct{}
}

// InOrder makes the background jobs started under ctx run in start order.
// Node started each with its first query at once, and the order those
// queries reached the pool decided the chat row ids; on a pgx pool a job
// that has to open a connection would lose that race to a later one.
func InOrder(ctx context.Context) context.Context {
	return context.WithValue(ctx, orderKey{}, &jobOrder{})
}

// later runs fn after the response, as Node's un-awaited calls ran: on its
// own context, which a client hanging up does not cancel.
func later(ctx context.Context, fn func(ctx context.Context)) {
	order, _ := ctx.Value(orderKey{}).(*jobOrder)
	if order == nil {
		db.Background(fn)
		return
	}
	order.mu.Lock()
	prev := order.last
	done := make(chan struct{})
	order.last = done
	order.mu.Unlock()
	db.Background(func(bctx context.Context) {
		defer close(done)
		if prev != nil {
			select {
			case <-prev:
			case <-bctx.Done():
				return
			}
		}
		fn(bctx)
	})
}

// background is later with a failure written to stderr as
// "<what>: <message>", as Node's .catch(console.error) did.
func (s *Service) background(ctx context.Context, what string, fn func(ctx context.Context) error) {
	later(ctx, func(ctx context.Context) {
		if err := fn(ctx); err != nil {
			s.Log.Errorf("%s: %s", what, err)
		}
	})
}

// vaTrigger is the payload Node's notifySystem sent for the virtual agents.
type vaTrigger struct {
	Type         string `json:"type"`
	DiscussionID ID     `json:"discussionId"`
	TriggerType  string `json:"triggerType"`
	VoteID       *ID    `json:"voteId,omitempty"`
}

// notifyVirtualAgents is notifySystem({type: 'virtual-agent', ...}) with
// its `.catch(() => {})`: started, never awaited, every failure dropped.
func (s *Service) notifyVirtualAgents(ctx context.Context, discussionID ID, trigger string, voteID *ID) {
	payload := vaTrigger{Type: "virtual-agent", DiscussionID: discussionID, TriggerType: trigger, VoteID: voteID}
	later(ctx, func(ctx context.Context) {
		s.Notifier.NotifySystem(ctx, payload)
	})
}

// sendEvent is sendDiscussionEvent(id, message).catch(log): the event line
// in the discussion, written after the response.
func (s *Service) sendEvent(ctx context.Context, discussionID ID, message, failure string) {
	param := discussionID.Param()
	if !discussionID.Truthy() {
		param = nil
	}
	s.background(ctx, failure, func(ctx context.Context) error {
		_, err := s.Notifier.SendDiscussionEvent(ctx, param, message)
		return err
	})
}

type participantState struct {
	agent, status, role string
}

// EvaluateReadiness moves a waiting discussion on: to active when every
// participant has joined, or at its timeout to active (every required
// participant joined; the optional ones still out time out) or timed_out
// (a required participant missing). Becoming active notifies the virtual
// agents. Anything but a waiting discussion is left alone.
func (s *Service) EvaluateReadiness(ctx context.Context, discussionID ID) error {
	var status string
	var timeoutAt *time.Time
	err := s.DB.QueryRow(ctx, "SELECT status, timeout_at FROM discussions WHERE id = $1", discussionID.Param()).Scan(&status, &timeoutAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status != "waiting" {
		return nil
	}

	rows, err := s.DB.Query(ctx,
		`SELECT ac.name AS agent, dp.status, dp.role
         FROM discussion_participants dp
         JOIN actors ac ON ac.id = dp.actor_id
         WHERE dp.discussion_id = $1`, discussionID.Param())
	if err != nil {
		return err
	}
	participants, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (participantState, error) {
		var p participantState
		err := row.Scan(&p.agent, &p.status, &p.role)
		return p, err
	})
	if err != nil {
		return err
	}
	allRequiredJoined, allOptionalJoined := true, true
	var optional []participantState
	for _, p := range participants {
		switch p.role {
		case "required":
			allRequiredJoined = allRequiredJoined && p.status == "joined"
		case "optional":
			allOptionalJoined = allOptionalJoined && p.status == "joined"
			optional = append(optional, p)
		}
	}

	if allRequiredJoined && allOptionalJoined {
		return s.activate(ctx, discussionID, "all_joined")
	}

	if timeoutAt == nil || s.now().Before(*timeoutAt) {
		return nil
	}
	if allRequiredJoined {
		for _, p := range optional {
			if p.status == "joined" {
				continue
			}
			actor, ok, err := s.Actors.ResolveByName(ctx, p.agent)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("Cannot read properties of null (reading 'id')")
			}
			if _, err := s.DB.Exec(ctx,
				"UPDATE discussion_participants SET status = $1 WHERE discussion_id = $2 AND actor_id = $3",
				"timed_out", discussionID.Param(), actor.ID); err != nil {
				return err
			}
		}
		return s.activate(ctx, discussionID, "timeout_required_present")
	}

	outcome, err := s.computeOutcome(ctx, discussionID.Param(), "timed_out")
	if err != nil {
		return err
	}
	if _, err := s.DB.Exec(ctx, "UPDATE discussions SET status = $1, outcome = $2 WHERE id = $3", "timed_out", outcome, discussionID.Param()); err != nil {
		return err
	}
	if err := s.leaveAll(ctx, discussionID.Param()); err != nil {
		return err
	}
	s.log("timed_out", logger.Fields{field("discussion_id", discussionID), field("reason", "missing_required"), field("outcome", outcome)})
	return nil
}

func (s *Service) activate(ctx context.Context, discussionID ID, reason string) error {
	if _, err := s.DB.Exec(ctx, "UPDATE discussions SET status = $1 WHERE id = $2", "active", discussionID.Param()); err != nil {
		return err
	}
	s.log("ready", logger.Fields{field("discussion_id", discussionID), field("reason", reason)})
	s.notifyVirtualAgents(ctx, discussionID, "discussion-active", nil)
	return nil
}

// leaveAll marks every participant still in a finished discussion as left.
func (s *Service) leaveAll(ctx context.Context, discussionID any) error {
	_, err := s.DB.Exec(ctx,
		`UPDATE discussion_participants SET status = 'left'
                 WHERE discussion_id = $1 AND status IN ('invited', 'joined', 'deferred')`, discussionID)
	return err
}

// evaluateThreshold: did choice 1 reach the threshold among participantCount
// voters? Any threshold but unanimous or majority never passes.
func evaluateThreshold(counts []choiceCount, participantCount int, threshold string) bool {
	var yes int64
	for _, c := range counts {
		if c.choice == 1 {
			yes = c.count
			break
		}
	}
	switch threshold {
	case "unanimous":
		return yes == int64(participantCount)
	case "majority":
		return float64(yes) > float64(participantCount)/2
	}
	return false
}

func totalBallots(counts []choiceCount) int64 {
	var total int64
	for _, c := range counts {
		total += c.count
	}
	return total
}

// EvaluateVote closes an open vote once every joined participant has voted
// or its closes_at has passed. A conclude vote that passed its threshold
// concludes the discussion. Anything else is left alone.
func (s *Service) EvaluateVote(ctx context.Context, voteID ID) error {
	var status, typ, threshold string
	var discussionID int
	var closesAt *time.Time
	err := s.DB.QueryRow(ctx,
		"SELECT v.status, v.discussion_id, v.closes_at, v.type, v.threshold FROM discussion_votes v JOIN discussions d ON v.discussion_id = d.id WHERE v.id = $1",
		voteID.Param()).Scan(&status, &discussionID, &closesAt, &typ, &threshold)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if status != "open" {
		return nil
	}

	participantCount, err := s.countJoined(ctx, discussionID)
	if err != nil {
		return err
	}
	counts, err := ballotCounts(ctx, s.DB, voteID.Param())
	if err != nil {
		return err
	}
	expired := closesAt != nil && !s.now().Before(*closesAt)
	allVoted := totalBallots(counts) >= int64(participantCount)
	if !allVoted && !expired {
		return nil
	}

	if _, err := s.DB.Exec(ctx, "UPDATE discussion_votes SET status = $1, closed_at = NOW() WHERE id = $2", "closed", voteID.Param()); err != nil {
		return err
	}
	if typ != "conclude" || !evaluateThreshold(counts, participantCount, threshold) {
		return nil
	}
	outcome, err := s.computeOutcome(ctx, discussionID, "concluded")
	if err != nil {
		return err
	}
	if _, err := s.DB.Exec(ctx,
		"UPDATE discussions SET status = $1, concluded_at = NOW(), outcome = $2 WHERE id = $3",
		"concluded", outcome, discussionID); err != nil {
		return err
	}
	if err := s.leaveAll(ctx, discussionID); err != nil {
		return err
	}
	s.log("auto_conclude", logger.Fields{field("discussion_id", discussionID), field("vote_id", voteID), field("outcome", outcome)})
	return nil
}

// countJoined is the number of joined participants (Node counted the rows
// of a SELECT).
func (s *Service) countJoined(ctx context.Context, discussionID int) (int, error) {
	rows, err := s.DB.Query(ctx,
		"SELECT actor_id FROM discussion_participants WHERE discussion_id = $1 AND status = $2", discussionID, "joined")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

// computeOutcome derives a finished discussion's outcome from its closed
// general votes: consensus when none failed, deadlock when none passed,
// else partial. A cancelled or timed-out discussion is abandoned.
func (s *Service) computeOutcome(ctx context.Context, discussionID any, newStatus string) (string, error) {
	if newStatus == "cancelled" || newStatus == "timed_out" {
		return "abandoned", nil
	}
	rows, err := s.DB.Query(ctx,
		"SELECT v.id, v.threshold FROM discussion_votes v WHERE v.discussion_id = $1 AND v.type = 'general' AND v.status = 'closed'",
		discussionID)
	if err != nil {
		return "", err
	}
	type closedVote struct {
		id        int
		threshold string
	}
	votes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (closedVote, error) {
		var v closedVote
		err := row.Scan(&v.id, &v.threshold)
		return v, err
	})
	if err != nil {
		return "", err
	}
	if len(votes) == 0 {
		return "consensus", nil
	}
	passed, failed := 0, 0
	for _, v := range votes {
		counts, err := ballotCounts(ctx, s.DB, v.id)
		if err != nil {
			return "", err
		}
		if evaluateThreshold(counts, int(totalBallots(counts)), v.threshold) {
			passed++
		} else {
			failed++
		}
	}
	switch {
	case failed == 0:
		return "consensus", nil
	case passed == 0:
		return "deadlock", nil
	}
	return "partial", nil
}

// joinCheck is a refused requireJoined: 403 with NOT_PARTICIPANT or
// NOT_JOINED.
type joinCheck struct {
	code, message string
}

var (
	notParticipant = &joinCheck{"NOT_PARTICIPANT", "Agent is not a participant in this discussion"}
	notJoined      = &joinCheck{"NOT_JOINED", "Agent has not joined this discussion"}
)

func (c *joinCheck) err() error {
	return apierr.WithCode(http.StatusForbidden, c.code, c.message)
}

// participantStatus is the agent's participant status, ok false when it is
// not a participant.
func (s *Service) participantStatus(ctx context.Context, discussionID any, actorID int) (status string, ok bool, err error) {
	err = s.DB.QueryRow(ctx,
		"SELECT status FROM discussion_participants WHERE discussion_id = $1 AND actor_id = $2",
		discussionID, actorID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return status, err == nil, err
}

// requireJoined is the service's check: an unknown agent is not a
// participant. (The vote/propose route's own copy answers 404 for it.)
func (s *Service) requireJoined(ctx context.Context, discussionID any, agent string) (*joinCheck, error) {
	actor, ok, err := s.Actors.ResolveByName(ctx, agent)
	if err != nil {
		return nil, err
	}
	if !ok {
		return notParticipant, nil
	}
	return s.joinedActor(ctx, discussionID, actor.ID)
}

func (s *Service) joinedActor(ctx context.Context, discussionID any, actorID int) (*joinCheck, error) {
	status, ok, err := s.participantStatus(ctx, discussionID, actorID)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return notParticipant, nil
	case status != "joined":
		return notJoined, nil
	}
	return nil, nil
}

// ConcludeResult is the answer to a conclude or cancel.
type ConcludeResult struct {
	DiscussionID ID     `json:"discussion_id"`
	Status       string `json:"status"`
	Outcome      string `json:"outcome"`
}

// Conclude ends a discussion the agent has joined: an active one is
// concluded (cancelled with cancel), a waiting one is cancelled, and only by
// its creator. The other participants get a system message and the
// discussion an event line, after the answer.
func (s *Service) Conclude(ctx context.Context, discussionID ID, agent string, cancel bool) (*ConcludeResult, error) {
	if !discussionID.Truthy() || agent == "" {
		return nil, apierr.New(http.StatusBadRequest, "Required fields: discussion_id, agent")
	}
	refused, err := s.requireJoined(ctx, discussionID.Param(), agent)
	if err != nil {
		return nil, err
	}
	if refused != nil {
		return nil, refused.err()
	}

	var status, topic string
	var createdByID int
	err = s.DB.QueryRow(ctx, "SELECT status, created_by_actor_id, topic FROM discussions WHERE id = $1", discussionID.Param()).Scan(&status, &createdByID, &topic)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apierr.New(http.StatusNotFound, "Discussion not found")
	}
	if err != nil {
		return nil, err
	}
	switch status {
	case "waiting":
		actor, err := s.Actors.RequireByName(ctx, agent)
		if err != nil {
			return nil, err
		}
		if createdByID != actor.ID {
			return nil, apierr.New(http.StatusForbidden, "Only the creator can cancel a waiting discussion")
		}
	case "active":
	default:
		return nil, apierr.New(http.StatusBadRequest, "Discussion is not active")
	}

	newStatus := "concluded"
	if status == "waiting" || cancel {
		newStatus = "cancelled"
	}
	outcome, err := s.computeOutcome(ctx, discussionID.Param(), newStatus)
	if err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(ctx, "UPDATE discussions SET status = $1, concluded_at = NOW(), outcome = $2 WHERE id = $3", newStatus, outcome, discussionID.Param()); err != nil {
		return nil, err
	}
	if err := s.leaveAll(ctx, discussionID.Param()); err != nil {
		return nil, err
	}
	action := "conclude"
	if newStatus == "cancelled" {
		action = "cancel"
	}
	s.log(action, logger.Fields{field("discussion_id", discussionID), field("agent", agent)})

	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx,
		`SELECT ac.name AS agent FROM discussion_participants dp
         JOIN actors ac ON ac.id = dp.actor_id
         WHERE dp.discussion_id = $1 AND dp.actor_id != $2`, discussionID.Param(), actor.ID)
	if err != nil {
		return nil, err
	}
	others, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	// Node started the participant messages, then the event, without
	// awaiting either. The event's text row comes first, because each
	// message looks its recipient up before inserting; one job keeps that
	// order. (Which delivery row Node wrote first was a race between the
	// two commits.)
	event := "Discussion " + newStatus + " by " + agent
	notice := "Discussion #" + discussionID.String() + ` ("` + topic + `") was ` + newStatus + " by " + agent
	param := discussionID.Param()
	later(ctx, func(ctx context.Context) {
		if _, err := s.Notifier.SendDiscussionEvent(ctx, param, event); err != nil {
			s.Log.Errorf("Failed to send conclude event: %s", err)
		}
		if len(others) > 0 {
			if _, err := s.Notifier.SendSystemMessageToMany(ctx, others, notice, nil); err != nil {
				s.Log.Errorf("Failed to notify participants of conclude: %s", err)
			}
		}
	})
	return &ConcludeResult{DiscussionID: discussionID, Status: newStatus, Outcome: outcome}, nil
}

// VoteCastResult is the answer to a ballot.
type VoteCastResult struct {
	VoteID     ID       `json:"vote_id"`
	Agent      string   `json:"agent"`
	Choice     jsNumber `json:"choice"`
	Reason     *string  `json:"reason"`
	VoteStatus string   `json:"vote_status"`
}

// VoteCast records the agent's ballot on an open vote of a discussion it has
// joined, then evaluates the vote. choice and reason are the JSON values
// sent: choice must be an integer number; reason a string, or falsy for
// none. The discussion gets an event line after the answer.
func (s *Service) VoteCast(ctx context.Context, voteID ID, agent string, choice, reason json.RawMessage) (*VoteCastResult, error) {
	if !voteID.Truthy() || agent == "" || isNullish(choice) {
		return nil, apierr.New(http.StatusBadRequest, "Required fields: vote_id, agent, choice")
	}
	n, isInteger := integerNumber(choice)
	if !isInteger {
		return nil, apierr.New(http.StatusBadRequest, "Choice must be an integer")
	}
	var reasonText *string
	if pgbind.Truthy(reason) {
		var text string
		if err := json.Unmarshal(reason, &text); err != nil {
			return nil, httpapi.NotString("reason")
		}
		reasonText = &text
	}

	var status, typ string
	var discussionID int
	err := s.DB.QueryRow(ctx, "SELECT status, discussion_id, type FROM discussion_votes WHERE id = $1", voteID.Param()).Scan(&status, &discussionID, &typ)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apierr.New(http.StatusNotFound, "Vote not found")
	}
	if err != nil {
		return nil, err
	}
	if status != "open" {
		return nil, apierr.New(http.StatusBadRequest, "Vote is not open")
	}
	refused, err := s.requireJoined(ctx, discussionID, agent)
	if err != nil {
		return nil, err
	}
	if refused != nil {
		return nil, refused.err()
	}
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return nil, err
	}
	var one int
	err = s.DB.QueryRow(ctx, "SELECT 1 FROM discussion_ballots WHERE vote_id = $1 AND actor_id = $2", voteID.Param(), actor.ID).Scan(&one)
	if err == nil {
		return nil, apierr.WithCode(http.StatusBadRequest, "ALREADY_VOTED", "Agent has already voted")
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if _, err := s.DB.Exec(ctx,
		"INSERT INTO discussion_ballots (vote_id, actor_id, choice, reason) VALUES ($1, $2, $3, $4)",
		voteID.Param(), actor.ID, jsutil.Num(n), reasonText); err != nil {
		return nil, err
	}
	if err := s.EvaluateVote(ctx, voteID); err != nil {
		return nil, err
	}
	var voteStatus string
	if err := s.DB.QueryRow(ctx, "SELECT status FROM discussion_votes WHERE id = $1", voteID.Param()).Scan(&voteStatus); err != nil {
		return nil, err
	}
	s.log("vote_cast", logger.Fields{field("vote_id", voteID), field("agent", agent), field("choice", jsNumber(n))})

	if typ == "" {
		typ = "general"
	}
	s.background(ctx, "Failed to send vote cast event", func(ctx context.Context) error {
		joined, err := s.countJoined(ctx, discussionID)
		if err != nil {
			return err
		}
		var cast int64
		if err := s.DB.QueryRow(ctx, "SELECT COUNT(*) as count FROM discussion_ballots WHERE vote_id = $1", voteID.Param()).Scan(&cast); err != nil {
			return err
		}
		msg := agent + " voted " + jsutil.NumberString(n) + " on " + typ + " vote #" + voteID.String() + "."
		if voteStatus == "closed" {
			msg += " Vote closed."
		} else {
			msg += " " + strconv.FormatInt(cast, 10) + "/" + strconv.Itoa(joined) + " votes cast."
		}
		_, err = s.Notifier.SendDiscussionEvent(ctx, discussionID, msg)
		return err
	})
	return &VoteCastResult{VoteID: voteID, Agent: agent, Choice: jsNumber(n), Reason: reasonText, VoteStatus: voteStatus}, nil
}

// isNullish is `x === undefined || x === null`.
func isNullish(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

// integerNumber is `typeof x === 'number' && Number.isInteger(x)`.
func integerNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || !(raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9') {
		return 0, false
	}
	n := jsutil.ToNumber(raw)
	if math.IsInf(n, 0) || math.IsNaN(n) || n != math.Trunc(n) {
		return 0, false
	}
	return n, true
}
