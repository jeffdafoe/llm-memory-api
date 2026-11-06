package discussion

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// The service functions below are the ones Node's MCP tools
// (routes/mcp.js), admin routes (routes/admin.js) and virtual agents
// (services/virtual-agent.js) call. Their checks throw, so every refusal is
// logged by the caller's error wrapper, and they differ from the REST
// routes' own copies where Node's did: create auto-joins virtual agents,
// leave re-evaluates open votes, propose notifies the virtual agents.

// ConflictError is create's 409: the creator is still in another waiting or
// active discussion.
type ConflictError struct {
	Err                  *apierr.Error
	ExistingDiscussionID int
}

func (e *ConflictError) Error() string { return e.Err.Error() }

// Unwrap lets apierr.StatusAndCode find the 409.
func (e *ConflictError) Unwrap() error { return e.Err }

func conflict(agent string, id int, status, topic string) *ConflictError {
	return &ConflictError{
		Err: apierr.WithCode(http.StatusConflict, "DISCUSSION_CONFLICT",
			`Agent "`+agent+`" is already in discussion #`+strconv.Itoa(id)+" ("+status+`): "`+topic+`"`),
		ExistingDiscussionID: id,
	}
}

// CreateInput is discussionCreate's arguments. Participants nil is a value
// that was not an array.
type CreateInput struct {
	Topic, CreatedBy       string
	Participants, Optional []string
	Channel, Mode, Context string
}

// CreatedParticipant is one entry of a new discussion's participant list.
type CreatedParticipant struct {
	Agent  string `json:"agent"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

// CreatedDiscussion is the discussion a create answers with.
type CreatedDiscussion struct {
	ID        int       `json:"id"`
	Topic     string    `json:"topic"`
	CreatedBy string    `json:"created_by"`
	Status    string    `json:"status"`
	Channel   *string   `json:"channel"`
	Mode      string    `json:"mode"`
	TimeoutAt Timestamp `json:"timeout_at"`
	CreatedAt Timestamp `json:"created_at"`
	Context   *string   `json:"context,omitempty"`
}

// CreateResult is the answer to a create.
type CreateResult struct {
	Discussion   CreatedDiscussion    `json:"discussion"`
	Participants []CreatedParticipant `json:"participants"`
}

// Create starts a discussion. The creator joins at once, and so does every
// virtual agent among the participants; the rest are invited and told so
// in the background.
func (s *Service) Create(ctx context.Context, in CreateInput) (*CreateResult, error) {
	if in.Topic == "" || in.CreatedBy == "" || in.Participants == nil {
		return nil, apierr.New(http.StatusBadRequest, "Required fields: topic, created_by, participants (array)")
	}
	optional := in.Optional
	if optional == nil {
		optional = []string{}
	}
	all := append(append([]string{}, in.Participants...), optional...)
	if len(all) < 2 {
		return nil, apierr.New(http.StatusBadRequest, "A discussion requires at least 2 participants")
	}
	if !contains(in.Participants, in.CreatedBy) {
		return nil, apierr.New(http.StatusBadRequest, "Creator must be in the participants list")
	}
	var overlap []string
	for _, a := range optional {
		if contains(in.Participants, a) {
			overlap = append(overlap, a)
		}
	}
	if len(overlap) > 0 {
		return nil, apierr.New(http.StatusBadRequest, "Agents cannot be both required and optional: "+strings.Join(overlap, ", "))
	}

	actorMap, err := s.Actors.ResolveMultipleByName(ctx, all)
	if err != nil {
		return nil, err
	}
	for _, name := range all {
		if _, ok := actorMap[name]; !ok {
			return nil, apierr.New(http.StatusNotFound, `Agent "`+name+`" is not registered`)
		}
	}
	creator := actorMap[in.CreatedBy]

	var existingID int
	var existingTopic, existingStatus string
	err = s.DB.QueryRow(ctx,
		`SELECT d.id, d.topic, d.status FROM discussions d
         JOIN discussion_participants dp ON dp.discussion_id = d.id
         WHERE dp.actor_id = $1 AND dp.status IN ('invited', 'joined', 'deferred')
         AND d.status IN ('waiting', 'active')
         ORDER BY d.id DESC LIMIT 1`, creator.ID).Scan(&existingID, &existingTopic, &existingStatus)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, err
	case existingStatus != "waiting":
		return nil, conflict(in.CreatedBy, existingID, existingStatus, existingTopic)
	default:
		if err := s.EvaluateReadiness(ctx, IntID(existingID)); err != nil {
			return nil, err
		}
		var now string
		err := s.DB.QueryRow(ctx, "SELECT status FROM discussions WHERE id = $1", existingID).Scan(&now)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if err != nil || now == "waiting" || now == "active" {
			return nil, conflict(in.CreatedBy, existingID, existingStatus, existingTopic)
		}
		s.log("conflict_cleared", logger.Fields{field("discussion_id", existingID), field("new_status", now), field("agent", in.CreatedBy)})
	}

	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	mode := "realtime"
	if in.Mode == "async" {
		mode = "async"
	}
	var storedContext *string
	if in.Context != "" {
		c := truncateUTF16Replacing(in.Context, contextLimit)
		storedContext = &c
	}
	timeoutKey := "discussion_wait_timeout_realtime"
	if mode == "async" {
		timeoutKey = "discussion_wait_timeout_async"
	}
	minutes, err := s.Config.Get(timeoutKey)
	if err != nil {
		return nil, err
	}
	timeoutAt, timeoutParam, _ := minutesFromNow(s.now(), minutes)
	var channel *string
	if in.Channel != "" {
		channel = &in.Channel
	}
	var id int
	var createdAt time.Time
	if err := tx.QueryRow(ctx,
		"INSERT INTO discussions (topic, created_by_actor_id, status, channel, mode, context, timeout_at) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, created_at",
		in.Topic, creator.ID, "waiting", channel, mode, storedContext, timeoutParam).Scan(&id, &createdAt); err != nil {
		return nil, err
	}

	actorIDs := make([]int, len(all))
	for i, name := range all {
		actorIDs[i] = actorMap[name].ID
	}
	rows, err := tx.Query(ctx, "SELECT actor_id FROM agent_configuration WHERE actor_id = ANY($1) AND virtual = TRUE", actorIDs)
	if err != nil {
		return nil, err
	}
	virtualIDs, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return nil, err
	}
	virtual := map[int]bool{}
	for _, v := range virtualIDs {
		virtual[v] = true
	}

	insert := func(agent, role string, join bool) error {
		status := "invited"
		var joinedAt *time.Time
		if join {
			status = "joined"
			t := jsDate(s.now())
			joinedAt = &t
		}
		_, err := tx.Exec(ctx,
			"INSERT INTO discussion_participants (discussion_id, actor_id, status, role, joined_at) VALUES ($1, $2, $3, $4, $5)",
			id, actorMap[agent].ID, status, role, joinedAt)
		return err
	}
	list := make([]CreatedParticipant, 0, len(all))
	for _, agent := range in.Participants {
		join := agent == in.CreatedBy || virtual[actorMap[agent].ID]
		if err := insert(agent, "required", join); err != nil {
			return nil, err
		}
		list = append(list, CreatedParticipant{Agent: agent, Role: "required", Status: joinStatus(join)})
	}
	for _, agent := range optional {
		join := virtual[actorMap[agent].ID]
		if err := insert(agent, "optional", join); err != nil {
			return nil, err
		}
		list = append(list, CreatedParticipant{Agent: agent, Role: "optional", Status: joinStatus(join)})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	s.log("create", logger.Fields{
		field("discussion_id", id), field("topic", in.Topic), field("created_by", in.CreatedBy),
		field("required", in.Participants), field("optional", optional), field("mode", mode),
	})
	var invited []string
	for _, a := range all {
		if a != in.CreatedBy {
			invited = append(invited, a)
		}
	}
	if len(invited) > 0 {
		topic, createdBy := in.Topic, in.CreatedBy
		s.background(ctx, "System notify error", func(ctx context.Context) error {
			_, err := s.Notifier.NotifyDiscussionInvite(ctx, strconv.Itoa(id), topic, createdBy, invited)
			return err
		})
	}
	if err := s.EvaluateReadiness(ctx, IntID(id)); err != nil {
		return nil, err
	}
	var status string
	if err := s.DB.QueryRow(ctx, "SELECT status FROM discussions WHERE id = $1", id).Scan(&status); err != nil {
		return nil, err
	}
	return &CreateResult{
		Discussion: CreatedDiscussion{
			ID: id, Topic: in.Topic, CreatedBy: in.CreatedBy, Status: status, Channel: channel, Mode: mode,
			TimeoutAt: Timestamp{timeoutAt}, CreatedAt: Timestamp{createdAt}, Context: storedContext,
		},
		Participants: list,
	}, nil
}

func joinStatus(joined bool) string {
	if joined {
		return "joined"
	}
	return "invited"
}

// creatorName is resolveById(created_by_actor_id)?.name, null when the actor
// is gone.
func (s *Service) creatorName(ctx context.Context, actorID int) (*string, error) {
	actor, ok, err := s.Actors.ResolveByID(ctx, actorID)
	if err != nil || !ok {
		return nil, err
	}
	return &actor.Name, nil
}

// List is discussionList: every discussion, or those the agent takes part
// in (none for an unknown agent), optionally of one status, newest first.
func (s *Service) List(ctx context.Context, status ID, agent string) ([]Discussion, error) {
	query := "SELECT " + discussionColumns + " FROM discussions d"
	var params []any
	var conditions []string
	if agent != "" {
		actor, ok, err := s.Actors.ResolveByName(ctx, agent)
		if err != nil {
			return nil, err
		}
		if !ok {
			return []Discussion{}, nil
		}
		query += " JOIN discussion_participants dp ON d.id = dp.discussion_id"
		params = append(params, actor.ID)
		conditions = append(conditions, "dp.actor_id = $"+strconv.Itoa(len(params)))
	}
	if status.Truthy() {
		params = append(params, status.Param())
		conditions = append(conditions, "d.status = $"+strconv.Itoa(len(params)))
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY d.created_at DESC"
	rows, err := s.DB.Query(ctx, query, params...)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DiscussionRow, error) {
		return scanDiscussionRow(row)
	})
	if err != nil {
		return nil, err
	}
	out := make([]Discussion, len(found))
	for i, d := range found {
		name, err := s.creatorName(ctx, d.CreatedByActorID)
		if err != nil {
			return nil, err
		}
		out[i] = Discussion{DiscussionRow: d, CreatedBy: name}
	}
	fields := logger.Fields{}
	if len(status.raw) > 0 {
		fields = append(fields, field("status", status))
	}
	if agent != "" {
		fields = append(fields, field("agent", agent))
	}
	s.log("list", append(fields, field("count", len(out))))
	return out, nil
}

// StatusResult is the answer to a status.
type StatusResult struct {
	Discussion   Discussion    `json:"discussion"`
	Participants []Participant `json:"participants"`
	Votes        []Vote        `json:"votes"`
}

// Status is discussionStatus: the discussion (its readiness and open votes
// evaluated first), its participants and its votes, newest first.
func (s *Service) Status(ctx context.Context, discussionID ID) (*StatusResult, error) {
	if !discussionID.Truthy() {
		return nil, apierr.New(http.StatusBadRequest, "Required field: discussion_id")
	}
	var status string
	err := s.DB.QueryRow(ctx, "SELECT status FROM discussions WHERE id = $1", discussionID.Param()).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apierr.New(http.StatusNotFound, "Discussion not found")
	}
	if err != nil {
		return nil, err
	}
	if status == "waiting" {
		if err := s.EvaluateReadiness(ctx, discussionID); err != nil {
			return nil, err
		}
	}
	participants, err := queryParticipants(ctx, s.DB, discussionID.Param())
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, "SELECT id, status FROM discussion_votes WHERE discussion_id = $1 ORDER BY created_at DESC", discussionID.Param())
	if err != nil {
		return nil, err
	}
	type voteState struct {
		id     int
		status string
	}
	votes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (voteState, error) {
		var v voteState
		err := row.Scan(&v.id, &v.status)
		return v, err
	})
	if err != nil {
		return nil, err
	}
	for _, v := range votes {
		if v.status == "open" {
			if err := s.EvaluateVote(ctx, IntID(v.id)); err != nil {
				return nil, err
			}
		}
	}
	d, err := scanDiscussionRow(s.DB.QueryRow(ctx, "SELECT "+discussionColumns+" FROM discussions d WHERE d.id = $1", discussionID.Param()))
	if err != nil {
		return nil, err
	}
	name, err := s.creatorName(ctx, d.CreatedByActorID)
	if err != nil {
		return nil, err
	}
	updated, err := queryVotes(ctx, s.DB, votesOfDiscussion, discussionID.Param())
	if err != nil {
		return nil, err
	}
	s.log("status", logger.Fields{field("discussion_id", discussionID)})
	return &StatusResult{Discussion: Discussion{DiscussionRow: d, CreatedBy: name}, Participants: participants, Votes: updated}, nil
}

// OpenVote is an open vote in the service's pending list (no proposer name).
type OpenVote struct {
	VoteRow
	DiscussionTopic string `json:"discussion_topic"`
	DiscussionMode  string `json:"discussion_mode"`
}

// PendingResult is the answer to a pending.
type PendingResult struct {
	Invited  []DiscussionRow `json:"invited_discussions"`
	Deferred []DiscussionRow `json:"deferred_discussions"`
	Votes    []OpenVote      `json:"open_votes"`
}

// Pending is discussionPending: the agent's invitations and deferrals in
// waiting or active discussions, and the open votes it has not cast in
// active discussions it joined (async ones, plus discussionID's when given).
func (s *Service) Pending(ctx context.Context, agent string, discussionID ID) (*PendingResult, error) {
	if agent == "" {
		return nil, apierr.New(http.StatusBadRequest, "Required field: agent")
	}
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return nil, err
	}
	byStatus := func(participantStatus string) ([]DiscussionRow, error) {
		rows, err := s.DB.Query(ctx, "SELECT "+discussionColumns+` FROM discussions d
         JOIN discussion_participants dp ON d.id = dp.discussion_id
         WHERE dp.actor_id = $1 AND dp.status = $2 AND d.status IN ($3, $4)`, actor.ID, participantStatus, "waiting", "active")
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DiscussionRow, error) {
			return scanDiscussionRow(row)
		})
	}
	invited, err := byStatus("invited")
	if err != nil {
		return nil, err
	}
	deferred, err := byStatus("deferred")
	if err != nil {
		return nil, err
	}
	params := []any{actor.ID, "joined", "open"}
	modeFilter := "AND d.mode = 'async'"
	if discussionID.Truthy() {
		modeFilter = "AND (d.mode = 'async' OR d.id = $4)"
		params = append(params, discussionID.Param())
	}
	rows, err := s.DB.Query(ctx, "SELECT "+voteColumns+`, d.topic as discussion_topic, d.mode as discussion_mode
         FROM discussion_votes v
         JOIN discussions d ON v.discussion_id = d.id
         JOIN discussion_participants dp ON d.id = dp.discussion_id
         WHERE dp.actor_id = $1
         AND dp.status = $2
         AND v.status = $3
         AND d.status = 'active'
         `+modeFilter+`
         AND NOT EXISTS (
             SELECT 1 FROM discussion_ballots b WHERE b.vote_id = v.id AND b.actor_id = $1
         )
         ORDER BY v.created_at DESC`, params...)
	if err != nil {
		return nil, err
	}
	votes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OpenVote, error) {
		var o OpenVote
		v, err := scanVoteRow(row, &o.DiscussionTopic, &o.DiscussionMode)
		o.VoteRow = v
		return o, err
	})
	if err != nil {
		return nil, err
	}
	s.log("pending", logger.Fields{field("agent", agent), field("invited", len(invited)), field("deferred", len(deferred)), field("open_votes", len(votes))})
	return &PendingResult{Invited: invited, Deferred: deferred, Votes: votes}, nil
}

// JoinResult is the answer to a join.
type JoinResult struct {
	DiscussionID     ID     `json:"discussion_id"`
	Agent            string `json:"agent"`
	Status           string `json:"status"`
	DiscussionStatus string `json:"discussion_status"`
	Message          string `json:"message,omitempty"`
}

// Join is discussionJoin: the agent joins a waiting or active discussion
// (a non-participant is added as required), which may make it active.
func (s *Service) Join(ctx context.Context, discussionID ID, agent string) (*JoinResult, error) {
	if !discussionID.Truthy() || agent == "" {
		return nil, apierr.New(http.StatusBadRequest, "Required fields: discussion_id, agent")
	}
	var status string
	err := s.DB.QueryRow(ctx, "SELECT status FROM discussions WHERE id = $1", discussionID.Param()).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apierr.New(http.StatusNotFound, "Discussion not found")
	}
	if err != nil {
		return nil, err
	}
	if status == "waiting" {
		if err := s.EvaluateReadiness(ctx, discussionID); err != nil {
			return nil, err
		}
	}
	if err := s.DB.QueryRow(ctx, "SELECT status FROM discussions WHERE id = $1", discussionID.Param()).Scan(&status); err != nil {
		return nil, err
	}
	if status != "waiting" && status != "active" {
		return nil, apierr.New(http.StatusBadRequest, "Discussion is "+status+" and not accepting joins")
	}
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return nil, err
	}
	current, ok, err := s.participantStatus(ctx, discussionID.Param(), actor.ID)
	if err != nil {
		return nil, err
	}
	switch {
	case ok && current == "joined":
		return &JoinResult{DiscussionID: discussionID, Agent: agent, Status: "joined", DiscussionStatus: status, Message: "Already joined"}, nil
	case ok:
		if _, err := s.DB.Exec(ctx,
			"UPDATE discussion_participants SET status = $1, joined_at = NOW() WHERE discussion_id = $2 AND actor_id = $3",
			"joined", discussionID.Param(), actor.ID); err != nil {
			return nil, err
		}
	default:
		if _, err := s.DB.Exec(ctx,
			"INSERT INTO discussion_participants (discussion_id, actor_id, status, role, joined_at) VALUES ($1, $2, $3, $4, NOW())",
			discussionID.Param(), actor.ID, "joined", "required"); err != nil {
			return nil, err
		}
	}
	s.log("join", logger.Fields{field("discussion_id", discussionID), field("agent", agent)})
	s.sendEvent(ctx, discussionID, agent+" joined the discussion", "Failed to send join event")
	if status == "waiting" {
		if err := s.EvaluateReadiness(ctx, discussionID); err != nil {
			return nil, err
		}
	}
	if err := s.DB.QueryRow(ctx, "SELECT status FROM discussions WHERE id = $1", discussionID.Param()).Scan(&status); err != nil {
		return nil, err
	}
	return &JoinResult{DiscussionID: discussionID, Agent: agent, Status: "joined", DiscussionStatus: status}, nil
}

// DeferResult is the answer to a defer.
type DeferResult struct {
	DiscussionID ID         `json:"discussion_id"`
	Agent        string     `json:"agent"`
	DeferredAt   *Timestamp `json:"deferred_at"`
	DeferCount   *int       `json:"defer_count"`
	TimeoutAt    Timestamp  `json:"timeout_at"`
}

// Defer is discussionDefer: an invited (or already deferred) participant
// puts the invitation off, up to max_defer_count times, and the
// discussion's timeout moves to discussion_defer_timeout minutes from now.
func (s *Service) Defer(ctx context.Context, discussionID ID, agent string) (*DeferResult, error) {
	if !discussionID.Truthy() || agent == "" {
		return nil, apierr.New(http.StatusBadRequest, "Required fields: discussion_id, agent")
	}
	var status string
	err := s.DB.QueryRow(ctx, "SELECT status FROM discussions WHERE id = $1", discussionID.Param()).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apierr.New(http.StatusNotFound, "Discussion not found")
	}
	if err != nil {
		return nil, err
	}
	if status != "waiting" && status != "active" {
		return nil, apierr.New(http.StatusBadRequest, "Discussion is "+status+" and not accepting deferrals")
	}
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return nil, err
	}
	var pStatus string
	var deferCount *int
	err = s.DB.QueryRow(ctx,
		"SELECT status, defer_count FROM discussion_participants WHERE discussion_id = $1 AND actor_id = $2",
		discussionID.Param(), actor.ID).Scan(&pStatus, &deferCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apierr.New(http.StatusNotFound, "Agent is not a participant in this discussion")
	}
	if err != nil {
		return nil, err
	}
	if pStatus != "invited" && pStatus != "deferred" {
		return nil, apierr.New(http.StatusBadRequest, "Can only defer from invited or deferred status, current status: "+pStatus)
	}
	maxText, err := s.Config.Get("max_defer_count")
	if err != nil {
		return nil, err
	}
	maxDefers, maxOK := jsutil.ParseInt(maxText)
	count := 0
	if deferCount != nil {
		count = *deferCount
	}
	if maxOK && float64(count) >= maxDefers {
		return nil, apierr.WithCode(http.StatusBadRequest, "MAX_DEFERRALS_REACHED",
			"Maximum deferrals reached ("+jsutil.NumberString(maxDefers)+"). Must join or let timeout expire.")
	}
	if _, err := s.DB.Exec(ctx,
		"UPDATE discussion_participants SET status = $1, deferred_at = NOW(), defer_count = defer_count + 1 WHERE discussion_id = $2 AND actor_id = $3",
		"deferred", discussionID.Param(), actor.ID); err != nil {
		return nil, err
	}
	timeoutText, err := s.Config.Get("discussion_defer_timeout")
	if err != nil {
		return nil, err
	}
	timeoutAt, timeoutParam, _ := minutesFromNow(s.now(), timeoutText)
	if _, err := s.DB.Exec(ctx, "UPDATE discussions SET timeout_at = $1 WHERE id = $2", timeoutParam, discussionID.Param()); err != nil {
		return nil, err
	}
	var deferredAt *time.Time
	var newCount *int
	if err := s.DB.QueryRow(ctx,
		"SELECT deferred_at, defer_count FROM discussion_participants WHERE discussion_id = $1 AND actor_id = $2",
		discussionID.Param(), actor.ID).Scan(&deferredAt, &newCount); err != nil {
		return nil, err
	}
	s.log("defer", logger.Fields{field("discussion_id", discussionID), field("agent", agent), field("defer_count", newCount), field("timeout_at", Timestamp{timeoutAt})})
	return &DeferResult{DiscussionID: discussionID, Agent: agent, DeferredAt: optionalTime(deferredAt), DeferCount: newCount, TimeoutAt: Timestamp{timeoutAt}}, nil
}

// LeaveResult is the answer to a leave.
type LeaveResult struct {
	DiscussionID ID     `json:"discussion_id"`
	Agent        string `json:"agent"`
	Status       string `json:"status"`
}

// Leave is discussionLeave: the participant leaves, and in the background,
// independently, the discussion gets an event line and its open votes are
// evaluated again (the leaver may have been the last one who had not voted). The virtual agents
// call it when one leaves a discussion.
func (s *Service) Leave(ctx context.Context, discussionID ID, agent string) (*LeaveResult, error) {
	if !discussionID.Truthy() || agent == "" {
		return nil, apierr.New(http.StatusBadRequest, "Required fields: discussion_id, agent")
	}
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return nil, err
	}
	_, ok, err := s.participantStatus(ctx, discussionID.Param(), actor.ID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, apierr.New(http.StatusNotFound, "Agent is not a participant in this discussion")
	}
	if _, err := s.DB.Exec(ctx,
		"UPDATE discussion_participants SET status = $1 WHERE discussion_id = $2 AND actor_id = $3",
		"left", discussionID.Param(), actor.ID); err != nil {
		return nil, err
	}
	s.log("leave", logger.Fields{field("discussion_id", discussionID), field("agent", agent)})
	s.sendEvent(ctx, discussionID, agent+" left the discussion", "Failed to send leave event")
	// Node started the re-evaluation without waiting for the event; it
	// writes no chat rows, so it runs beside it.
	later(unordered(ctx), func(ctx context.Context) {
		if err := s.reEvaluateOpenVotes(ctx, discussionID); err != nil {
			s.log("leave-vote-reeval-error", logger.Fields{field("discussion_id", discussionID), field("error", err.Error())})
		}
	})
	return &LeaveResult{DiscussionID: discussionID, Agent: agent, Status: "left"}, nil
}

func (s *Service) reEvaluateOpenVotes(ctx context.Context, discussionID ID) error {
	rows, err := s.DB.Query(ctx, "SELECT id FROM discussion_votes WHERE discussion_id = $1 AND status = $2", discussionID.Param(), "open")
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.EvaluateVote(ctx, IntID(id)); err != nil {
			return err
		}
	}
	return nil
}

// ProposeInput is votePropose's arguments: the JSON values the caller
// sent for type, threshold and closes_at.
type ProposeInput struct {
	DiscussionID         ID
	ProposedBy, Question string
	Type, Threshold      ID
	ClosesAt             ID
}

// ProposedVote is the vote a propose answers with.
type ProposedVote struct {
	ID           int       `json:"id"`
	DiscussionID ID        `json:"discussion_id"`
	ProposedBy   string    `json:"proposed_by"`
	Question     string    `json:"question"`
	Type         string    `json:"type"`
	Threshold    string    `json:"threshold"`
	ClosesAt     ID        `json:"closes_at"`
	CreatedAt    Timestamp `json:"created_at"`
}

// ProposeResult is the answer to a propose.
type ProposeResult struct {
	Vote ProposedVote `json:"vote"`
}

// kind is `x || fallback` checked against the allowed values; a value of
// another JSON type never matches.
func kind(v ID, fallback string, allowed ...string) (string, bool) {
	if !v.Truthy() {
		return fallback, true
	}
	s, isString := v.Param().(string)
	if v.raw[0] != '"' {
		isString = false
	}
	return s, isString && contains(allowed, s)
}

// Propose is votePropose: a joined participant opens a vote in an active or
// waiting discussion. The discussion gets an event line and the virtual
// agents are told to vote.
func (s *Service) Propose(ctx context.Context, in ProposeInput) (*ProposeResult, error) {
	if !in.DiscussionID.Truthy() || in.ProposedBy == "" || in.Question == "" {
		return nil, apierr.New(http.StatusBadRequest, "Required fields: discussion_id, proposed_by, question")
	}
	refused, err := s.requireJoined(ctx, in.DiscussionID.Param(), in.ProposedBy)
	if err != nil {
		return nil, err
	}
	if refused != nil {
		return nil, refused.err()
	}
	var status string
	err = s.DB.QueryRow(ctx, "SELECT status FROM discussions WHERE id = $1", in.DiscussionID.Param()).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errors.New("Cannot read properties of undefined (reading 'status')")
	}
	if err != nil {
		return nil, err
	}
	if status != "active" && status != "waiting" {
		return nil, apierr.New(http.StatusBadRequest, "Discussion is not active or waiting")
	}
	voteType, ok := kind(in.Type, "general", "general", "conclude")
	if !ok {
		return nil, apierr.New(http.StatusBadRequest, `Vote type must be "general" or "conclude"`)
	}
	threshold, ok := kind(in.Threshold, "unanimous", "unanimous", "majority")
	if !ok {
		return nil, apierr.New(http.StatusBadRequest, `Threshold must be "unanimous" or "majority"`)
	}
	closesAt := in.ClosesAt
	var closesParam any
	if closesAt.Truthy() {
		closesParam = closesAt.Param()
	} else {
		closesAt = RawID([]byte("null"))
	}
	proposer, err := s.Actors.RequireByName(ctx, in.ProposedBy)
	if err != nil {
		return nil, err
	}
	var voteID int
	var createdAt time.Time
	if err := s.DB.QueryRow(ctx,
		"INSERT INTO discussion_votes (discussion_id, proposed_by_actor_id, question, type, threshold, closes_at) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at",
		in.DiscussionID.Param(), proposer.ID, in.Question, voteType, threshold, closesParam).Scan(&voteID, &createdAt); err != nil {
		return nil, err
	}
	s.log("vote_propose", logger.Fields{field("discussion_id", in.DiscussionID), field("vote_id", voteID), field("proposed_by", in.ProposedBy), field("question", in.Question), field("type", voteType)})
	s.sendEvent(ctx, in.DiscussionID, in.ProposedBy+" proposed "+voteType+" vote #"+strconv.Itoa(voteID)+": "+in.Question, "Failed to send vote propose event")
	vote := IntID(voteID)
	// Node started the notification without waiting for the event, so a
	// stalled event does not hold the virtual agents' vote; which of the
	// two texts inserts first is a race, in Node too.
	s.notifyVirtualAgents(unordered(ctx), in.DiscussionID, "vote-proposed", &vote)
	return &ProposeResult{Vote: ProposedVote{
		ID: voteID, DiscussionID: in.DiscussionID, ProposedBy: in.ProposedBy, Question: in.Question,
		Type: voteType, Threshold: threshold, ClosesAt: closesAt, CreatedAt: Timestamp{createdAt},
	}}, nil
}

// VoteStatusResult is the answer to a vote status; Vote is nil when the
// proposer is gone (Node's rows[0] was undefined, which JSON.stringify
// leaves out).
type VoteStatusResult struct {
	Vote    *Vote    `json:"vote,omitempty"`
	Ballots []Ballot `json:"ballots"`
}

// VoteStatus is voteStatus: the vote (evaluated first when open) with its
// ballots, oldest first.
func (s *Service) VoteStatus(ctx context.Context, voteID ID) (*VoteStatusResult, error) {
	if !voteID.Truthy() {
		return nil, apierr.New(http.StatusBadRequest, "Required field: vote_id")
	}
	var status string
	err := s.DB.QueryRow(ctx, "SELECT status FROM discussion_votes WHERE id = $1", voteID.Param()).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apierr.New(http.StatusNotFound, "Vote not found")
	}
	if err != nil {
		return nil, err
	}
	if status == "open" {
		if err := s.EvaluateVote(ctx, voteID); err != nil {
			return nil, err
		}
	}
	return s.voteWithBallots(ctx, voteID)
}

func (s *Service) voteWithBallots(ctx context.Context, voteID ID) (*VoteStatusResult, error) {
	votes, err := queryVotes(ctx, s.DB, "SELECT "+voteColumns+`, ac.name AS proposed_by
         FROM discussion_votes v
         JOIN actors ac ON ac.id = v.proposed_by_actor_id
         WHERE v.id = $1`, voteID.Param())
	if err != nil {
		return nil, err
	}
	ballots, err := queryBallots(ctx, s.DB, voteID.Param())
	if err != nil {
		return nil, err
	}
	s.log("vote_status", logger.Fields{field("vote_id", voteID)})
	res := &VoteStatusResult{Ballots: ballots}
	if len(votes) > 0 {
		res.Vote = &votes[0]
	}
	return res, nil
}
