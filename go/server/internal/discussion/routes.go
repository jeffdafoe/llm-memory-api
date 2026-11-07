package discussion

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sanitize"
)

// Routes are the /v1/discussion routes (Node's routes/discussion.js).
// conclude, cancel and vote/cast call the service; the others carry their
// own steps, as in Node: create does not auto-join virtual agents,
// vote/propose does not notify them, and leave does not re-evaluate open
// votes.
type Routes struct {
	Service *Service
	Router  *httpapi.Router
}

// Register adds the routes to the /v1 mux.
func (rt *Routes) Register(mux *httpapi.RawMux) {
	route := func(path, label string, fn httpapi.HandlerFunc) {
		mux.Handle(http.MethodPost, "/discussion/"+path, rt.Router.Route("discussion", label, fn))
	}
	route("create", "create", rt.create)
	route("list", "list", rt.list)
	route("status", "status", rt.status)
	route("pending", "pending", rt.pending)
	route("conclude", "conclude", rt.conclude)
	route("cancel", "cancel", rt.cancel)
	route("join", "join", rt.join)
	route("defer", "defer", rt.deferInvite)
	route("leave", "leave", rt.leave)
	route("vote/propose", "vote-propose", rt.voteProposeRoute)
	route("vote/cast", "vote-cast", rt.voteCast)
	route("vote/status", "vote-status", rt.voteStatus)
}

// routeTimeout bounds a route's database work once it no longer follows the
// request: Node finished a route whose client had gone away, so the writes
// here do too.
const routeTimeout = 30 * time.Second

func detached(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(InOrder(context.WithoutCancel(r.Context())), routeTimeout)
}

// noBodyRead is V8's TypeError for `req.body.<field>` with no JSON body.
func noBodyRead(field string) error {
	return errors.New("Cannot read properties of undefined (reading '" + field + "')")
}

func writeError(w http.ResponseWriter, status int, code, message string) error {
	httpapi.WriteError(w, status, code, message)
	return nil
}

// agentIdentity reads the `agent` field as every route but create and
// vote/propose does.
func agentIdentity(w http.ResponseWriter, r *http.Request, f httpapi.Fields) (string, bool, error) {
	return httpapi.Identity(w, r, f, "agent", "agent does not match authenticated agent")
}

// contentField is a free-text field after sanitize.content: its text and
// JavaScript truthiness. A value of another JSON type is kept raw, as
// sanitize.content returned it; truthy reports its truthiness and the
// caller refuses it once the required check has passed.
func contentField(f httpapi.Fields, name string) (text string, isString, truthy bool) {
	if s, ok := f.String(name); ok {
		text = sanitize.Content(s)
		return text, true, text != ""
	}
	return "", false, f.Truthy(name)
}

// agentNames is an array of agent names, sanitized. A non-string element is
// refused (Node looked it up as text and answered 404).
func agentNames(elems []json.RawMessage, field string) ([]string, error) {
	out := make([]string, len(elems))
	for i, e := range elems {
		var s string
		if len(e) == 0 || e[0] != '"' || json.Unmarshal(e, &s) != nil {
			return nil, apierr.New(http.StatusBadRequest, field+" must be an array of strings")
		}
		out[i] = sanitize.AgentName(s)
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

type createdDiscussion struct {
	ID        int     `json:"id"`
	Topic     string  `json:"topic"`
	CreatedBy string  `json:"created_by"`
	Status    string  `json:"status"`
	Channel   *string `json:"channel"`
	Mode      string  `json:"mode"`
	TimeoutAt string  `json:"timeout_at"`
	CreatedAt string  `json:"created_at"`
	Context   *string `json:"context,omitempty"`
}

type createdParticipant struct {
	Agent  string `json:"agent"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

// contextLimit is the stored context's length in UTF-16 units.
const contextLimit = 10000

func (rt *Routes) create(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("mode")
	}
	ctx, cancel := detached(r)
	defer cancel()
	s := rt.Service

	createdBy, done, err := httpapi.Identity(w, r, f, "created_by", "created_by does not match authenticated agent")
	if done || err != nil {
		return err
	}
	topic, topicIsString, topicTruthy := contentField(f, "topic")
	participantsRaw, isArray := f.Array("participants")
	if !topicTruthy || createdBy == "" || !isArray {
		return httpapi.BadRequest(w, "Required fields: topic, created_by, participants (array)")
	}
	if !topicIsString {
		return httpapi.NotString("topic")
	}
	channel, err := f.OptionalString("channel")
	if err != nil {
		return err
	}
	channel = sanitize.Identifier(channel)
	contextText, err := f.OptionalString("context")
	if err != nil {
		return err
	}
	contextText = sanitize.Content(contextText)
	participants, err := agentNames(participantsRaw, "participants")
	if err != nil {
		return err
	}
	optional := []string{}
	if raw, ok := f.Array("optional_participants"); ok {
		if optional, err = agentNames(raw, "optional_participants"); err != nil {
			return err
		}
	}
	all := append(append([]string{}, participants...), optional...)

	if len(all) < 2 {
		return httpapi.BadRequest(w, "A discussion requires at least 2 participants")
	}
	if !contains(participants, createdBy) {
		return httpapi.BadRequest(w, "Creator must be in the participants list")
	}
	var overlap []string
	for _, a := range optional {
		if contains(participants, a) {
			overlap = append(overlap, a)
		}
	}
	if len(overlap) > 0 {
		return httpapi.BadRequest(w, "Agents cannot be both required and optional: "+strings.Join(overlap, ", "))
	}

	creator, err := s.Actors.RequireByName(ctx, createdBy)
	if err != nil {
		return err
	}
	// A creator still in a waiting or active discussion is refused. A
	// waiting one is evaluated first: it may have timed out since anyone
	// looked.
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
		return err
	default:
		cleared := false
		if existingStatus == "waiting" {
			if err := s.EvaluateReadiness(ctx, IntID(existingID)); err != nil {
				return err
			}
			var now string
			err := s.DB.QueryRow(ctx, "SELECT status FROM discussions WHERE id = $1", existingID).Scan(&now)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err == nil && now != "waiting" && now != "active" {
				s.log("conflict_cleared", logger.Fields{field("discussion_id", existingID), field("new_status", now), field("agent", createdBy)})
				cleared = true
			}
		}
		if !cleared {
			return httpapi.WriteJSON(w, http.StatusConflict, conflictBody{Error: conflictError{
				Code:                 "DISCUSSION_CONFLICT",
				Message:              `Agent "` + createdBy + `" is already in discussion #` + strconv.Itoa(existingID) + " (" + existingStatus + `): "` + existingTopic + `"`,
				ExistingDiscussionID: existingID,
			}})
		}
	}

	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	mode := "realtime"
	if m, ok := f.String("mode"); ok && (m == "realtime" || m == "async") {
		mode = m
	}
	var storedContext *string
	if contextText != "" {
		c := truncateUTF16Replacing(contextText, contextLimit)
		storedContext = &c
	}
	timeoutKey := "discussion_wait_timeout_realtime"
	if mode == "async" {
		timeoutKey = "discussion_wait_timeout_async"
	}
	minutes, err := s.Config.Get(timeoutKey)
	if err != nil {
		return err
	}
	timeoutAt, timeoutParam, _ := minutesFromNow(s.now(), minutes)
	var channelParam *string
	if channel != "" {
		channelParam = &channel
	}

	var id int
	var createdAt time.Time
	if err := tx.QueryRow(ctx,
		"INSERT INTO discussions (topic, created_by_actor_id, status, channel, mode, context, timeout_at) VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id, created_at",
		topic, creator.ID, "waiting", channelParam, mode, storedContext, timeoutParam).Scan(&id, &createdAt); err != nil {
		return err
	}
	// The names are looked up on the pool, outside the transaction, as
	// Node's requireByName was; an unknown one rolls the insert back.
	for _, agent := range participants {
		actorID := creator.ID
		status := "invited"
		var joinedAt *time.Time
		if agent == createdBy {
			status = "joined"
			t := jsDate(s.now())
			joinedAt = &t
		} else {
			actor, err := s.Actors.RequireByName(ctx, agent)
			if err != nil {
				return err
			}
			actorID = actor.ID
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO discussion_participants (discussion_id, actor_id, status, role, joined_at) VALUES ($1, $2, $3, $4, $5)",
			id, actorID, status, "required", joinedAt); err != nil {
			return err
		}
	}
	for _, agent := range optional {
		actor, err := s.Actors.RequireByName(ctx, agent)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO discussion_participants (discussion_id, actor_id, status, role, joined_at) VALUES ($1, $2, $3, $4, $5)",
			id, actor.ID, "invited", "optional", nil); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	s.log("create", logger.Fields{
		field("discussion_id", id), field("topic", topic), field("created_by", createdBy),
		field("required", participants), field("optional", optional), field("mode", mode),
	})

	var invited []string
	for _, a := range all {
		if a != createdBy {
			invited = append(invited, a)
		}
	}
	if len(invited) > 0 {
		s.background(ctx, "System notify error", func(ctx context.Context) error {
			_, err := s.Notifier.NotifyDiscussionInvite(ctx, strconv.Itoa(id), topic, createdBy, invited)
			return err
		})
	}

	if err := s.EvaluateReadiness(ctx, IntID(id)); err != nil {
		return err
	}
	var status string
	if err := s.DB.QueryRow(ctx, "SELECT status FROM discussions WHERE id = $1", id).Scan(&status); err != nil {
		return err
	}

	list := make([]createdParticipant, 0, len(all))
	for _, a := range participants {
		st := "invited"
		if a == createdBy {
			st = "joined"
		}
		list = append(list, createdParticipant{Agent: a, Role: "required", Status: st})
	}
	for _, a := range optional {
		list = append(list, createdParticipant{Agent: a, Role: "optional", Status: "invited"})
	}
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Discussion   createdDiscussion    `json:"discussion"`
		Participants []createdParticipant `json:"participants"`
	}{
		Discussion: createdDiscussion{
			ID: id, Topic: topic, CreatedBy: createdBy, Status: status, Channel: channelParam, Mode: mode,
			TimeoutAt: jsutil.DateISO(timeoutAt), CreatedAt: jsutil.DateISO(createdAt), Context: storedContext,
		},
		Participants: list,
	})
}

type conflictError struct {
	Code                 string `json:"code"`
	Message              string `json:"message"`
	ExistingDiscussionID int    `json:"existing_discussion_id"`
}

type conflictBody struct {
	Error conflictError `json:"error"`
}

// truncateUTF16Replacing is String(s).slice(0, max) as it reached Postgres:
// a surrogate pair split at the cut leaves a lone high surrogate, which
// node-postgres encoded as U+FFFD.
func truncateUTF16Replacing(s string, max int) string {
	cut := jsutil.TruncateUTF16(s, max)
	if len(cut) < len(s) && jsutil.UTF16Len(cut) < max {
		return cut + "�"
	}
	return cut
}

func (rt *Routes) list(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("status")
	}
	ctx, cancel := detached(r)
	defer cancel()
	s := rt.Service

	// An agent session lists its own discussions; the agent field is read
	// only for an admin.
	scoped := httpapi.StateOf(r).AuthenticatedAgent
	if scoped == "" {
		agent, err := httpapi.AgentField(f, "agent")
		if err != nil {
			return err
		}
		scoped = agent
	}

	query := "SELECT " + discussionColumns + `, ac.name AS created_by FROM discussions d
                 JOIN actors ac ON ac.id = d.created_by_actor_id`
	var params []any
	var conditions []string
	if scoped != "" {
		actor, err := s.Actors.RequireByName(ctx, scoped)
		if err != nil {
			return err
		}
		query += " JOIN discussion_participants dp ON d.id = dp.discussion_id"
		params = append(params, actor.ID)
		conditions = append(conditions, "dp.actor_id = $"+strconv.Itoa(len(params)))
	}
	status := RawID(f.Raw("status"))
	if status.Truthy() {
		params = append(params, status.Param())
		conditions = append(conditions, "d.status = $"+strconv.Itoa(len(params)))
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY d.created_at DESC"

	discussions, err := queryDiscussions(ctx, s.DB, query, params...)
	if err != nil {
		return err
	}
	fields := logger.Fields{}
	if raw := f.Raw("status"); raw != nil {
		fields = append(fields, field("status", status))
	}
	if scoped != "" || f.Raw("agent") != nil || httpapi.StateOf(r).AuthenticatedAgent != "" {
		fields = append(fields, field("agent", scoped))
	}
	s.log("list", append(fields, field("count", len(discussions))))
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Discussions []Discussion `json:"discussions"`
	}{discussions})
}

// participantOf reports whether the agent is any kind of participant (the
// status and vote/status check for an agent session).
func (s *Service) participantOf(ctx context.Context, discussionID any, agent string) (bool, error) {
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return false, err
	}
	var one int
	err = s.DB.QueryRow(ctx, "SELECT 1 FROM discussion_participants WHERE discussion_id = $1 AND actor_id = $2", discussionID, actor.ID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

const votesOfDiscussion = "SELECT " + voteColumns + `, ac.name AS proposed_by
         FROM discussion_votes v
         JOIN actors ac ON ac.id = v.proposed_by_actor_id
         WHERE v.discussion_id = $1 ORDER BY v.created_at DESC`

func (rt *Routes) status(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("discussion_id")
	}
	ctx, cancel := detached(r)
	defer cancel()
	s := rt.Service

	id := RawID(f.Raw("discussion_id"))
	if !id.Truthy() {
		return httpapi.BadRequest(w, "Required field: discussion_id")
	}
	var status string
	err := s.DB.QueryRow(ctx, "SELECT status FROM discussions WHERE id = $1", id.Param()).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return writeError(w, http.StatusNotFound, "NOT_FOUND", "Discussion not found")
	}
	if err != nil {
		return err
	}
	if agent := httpapi.StateOf(r).AuthenticatedAgent; agent != "" {
		ok, err := s.participantOf(ctx, id.Param(), agent)
		if err != nil {
			return err
		}
		if !ok {
			return writeError(w, http.StatusForbidden, "FORBIDDEN", "Not a participant in this discussion")
		}
	}
	if status == "waiting" {
		if err := s.EvaluateReadiness(ctx, id); err != nil {
			return err
		}
	}

	participants, err := queryParticipants(ctx, s.DB, id.Param())
	if err != nil {
		return err
	}
	votes, err := queryVotes(ctx, s.DB, votesOfDiscussion, id.Param())
	if err != nil {
		return err
	}
	for _, v := range votes {
		if v.Status == "open" {
			if err := s.EvaluateVote(ctx, IntID(v.ID)); err != nil {
				return err
			}
		}
	}
	discussions, err := queryDiscussions(ctx, s.DB, "SELECT "+discussionColumns+`, ac.name AS created_by FROM discussions d
         JOIN actors ac ON ac.id = d.created_by_actor_id
         WHERE d.id = $1`, id.Param())
	if err != nil {
		return err
	}
	if votes, err = queryVotes(ctx, s.DB, votesOfDiscussion, id.Param()); err != nil {
		return err
	}
	s.log("status", logger.Fields{field("discussion_id", id)})

	// rows[0] of a discussion whose creator is gone is undefined, which
	// JSON.stringify leaves out.
	var discussion *Discussion
	if len(discussions) > 0 {
		discussion = &discussions[0]
	}
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Discussion   *Discussion   `json:"discussion,omitempty"`
		Participants []Participant `json:"participants"`
		Votes        []Vote        `json:"votes"`
	}{discussion, participants, votes})
}

func (rt *Routes) pending(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBodyRead("agent")
	}
	ctx, cancel := detached(r)
	defer cancel()
	s := rt.Service

	agent, done, err := agentIdentity(w, r, f)
	if done || err != nil {
		return err
	}
	if agent == "" {
		return httpapi.BadRequest(w, "Required field: agent")
	}
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return err
	}
	byParticipantStatus := "SELECT " + discussionColumns + `, ac.name AS created_by FROM discussions d
         JOIN actors ac ON ac.id = d.created_by_actor_id
         JOIN discussion_participants dp ON d.id = dp.discussion_id
         WHERE dp.actor_id = $1 AND dp.status = $2 AND d.status IN ($3, $4)`
	invited, err := queryDiscussions(ctx, s.DB, byParticipantStatus, actor.ID, "invited", "waiting", "active")
	if err != nil {
		return err
	}
	deferred, err := queryDiscussions(ctx, s.DB, byParticipantStatus, actor.ID, "deferred", "waiting", "active")
	if err != nil {
		return err
	}

	// Realtime discussions' votes reach their participants in the chat, not
	// here, unless the caller names that discussion (the transport polling
	// its own).
	params := []any{actor.ID, "joined", "open"}
	modeFilter := "AND d.mode = 'async'"
	discussionID := RawID(f.Raw("discussion_id"))
	if discussionID.Truthy() {
		modeFilter = "AND (d.mode = 'async' OR d.id = $4)"
		params = append(params, discussionID.Param())
	}
	rows, err := s.DB.Query(ctx, "SELECT "+voteColumns+`, pac.name AS proposed_by, d.topic as discussion_topic, d.mode as discussion_mode
         FROM discussion_votes v
         JOIN actors pac ON pac.id = v.proposed_by_actor_id
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
		return err
	}
	openVotes, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (PendingVote, error) {
		var p PendingVote
		v, err := scanVoteRow(row, &p.ProposedBy, &p.DiscussionTopic, &p.DiscussionMode)
		p.VoteRow = v
		return p, err
	})
	if err != nil {
		return err
	}
	s.log("pending", logger.Fields{field("agent", agent), field("invited", len(invited)), field("deferred", len(deferred)), field("open_votes", len(openVotes))})
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Invited  []Discussion  `json:"invited_discussions"`
		Deferred []Discussion  `json:"deferred_discussions"`
		Votes    []PendingVote `json:"open_votes"`
	}{invited, deferred, openVotes})
}

func (rt *Routes) conclude(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBodyRead("agent")
	}
	ctx, cancel := detached(r)
	defer cancel()
	agent, done, err := agentIdentity(w, r, f)
	if done || err != nil {
		return err
	}
	res, err := rt.Service.Conclude(ctx, RawID(f.Raw("discussion_id")), agent, false)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) cancel(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBodyRead("agent")
	}
	ctx, cancel := detached(r)
	defer cancel()
	agent, done, err := agentIdentity(w, r, f)
	if done || err != nil {
		return err
	}
	id := RawID(f.Raw("discussion_id"))
	if !id.Truthy() || agent == "" {
		return httpapi.BadRequest(w, "Required fields: discussion_id, agent")
	}
	res, err := rt.Service.Conclude(ctx, id, agent, true)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

// agentAndDiscussion reads the body of join, defer and leave: the agent and
// the discussion id, both required.
func agentAndDiscussion(w http.ResponseWriter, r *http.Request) (agent string, id ID, done bool, err error) {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return "", id, true, noBodyRead("agent")
	}
	agent, done, err = agentIdentity(w, r, f)
	if done || err != nil {
		return "", id, true, err
	}
	id = RawID(f.Raw("discussion_id"))
	if !id.Truthy() || agent == "" {
		return "", id, true, httpapi.BadRequest(w, "Required fields: discussion_id, agent")
	}
	return agent, id, false, nil
}

type joinResult struct {
	DiscussionID     ID     `json:"discussion_id"`
	Agent            string `json:"agent"`
	Status           string `json:"status"`
	DiscussionStatus string `json:"discussion_status"`
	Message          string `json:"message,omitempty"`
}

func (rt *Routes) join(w http.ResponseWriter, r *http.Request) error {
	agent, id, done, err := agentAndDiscussion(w, r)
	if done || err != nil {
		return err
	}
	ctx, cancel := detached(r)
	defer cancel()
	s := rt.Service

	var status string
	err = s.DB.QueryRow(ctx, "SELECT status FROM discussions WHERE id = $1", id.Param()).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return writeError(w, http.StatusNotFound, "NOT_FOUND", "Discussion not found")
	}
	if err != nil {
		return err
	}
	if status == "waiting" {
		if err := s.EvaluateReadiness(ctx, id); err != nil {
			return err
		}
	}
	if err := s.DB.QueryRow(ctx, "SELECT status FROM discussions WHERE id = $1", id.Param()).Scan(&status); err != nil {
		return err
	}
	if status != "waiting" && status != "active" {
		return httpapi.BadRequest(w, "Discussion is "+status+" and not accepting joins")
	}

	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return err
	}
	current, ok, err := s.participantStatus(ctx, id.Param(), actor.ID)
	if err != nil {
		return err
	}
	switch {
	case ok && current == "joined":
		return httpapi.WriteJSON(w, http.StatusOK, joinResult{DiscussionID: id, Agent: agent, Status: "joined", DiscussionStatus: status, Message: "Already joined"})
	case ok:
		// timed_out, invited and left participants may join again.
		if _, err := s.DB.Exec(ctx,
			"UPDATE discussion_participants SET status = $1, joined_at = NOW() WHERE discussion_id = $2 AND actor_id = $3",
			"joined", id.Param(), actor.ID); err != nil {
			return err
		}
	default:
		if _, err := s.DB.Exec(ctx,
			"INSERT INTO discussion_participants (discussion_id, actor_id, status, role, joined_at) VALUES ($1, $2, $3, $4, NOW())",
			id.Param(), actor.ID, "joined", "required"); err != nil {
			return err
		}
	}
	s.log("join", logger.Fields{field("discussion_id", id), field("agent", agent)})
	// Written before readiness, as in Service.Join.
	s.sendEventNow(ctx, id, agent+" joined the discussion", "Failed to send join event")

	if status == "waiting" {
		if err := s.EvaluateReadiness(ctx, id); err != nil {
			return err
		}
	}
	if err := s.DB.QueryRow(ctx, "SELECT status FROM discussions WHERE id = $1", id.Param()).Scan(&status); err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, joinResult{DiscussionID: id, Agent: agent, Status: "joined", DiscussionStatus: status})
}

func (rt *Routes) deferInvite(w http.ResponseWriter, r *http.Request) error {
	agent, id, done, err := agentAndDiscussion(w, r)
	if done || err != nil {
		return err
	}
	ctx, cancel := detached(r)
	defer cancel()
	s := rt.Service

	var status string
	err = s.DB.QueryRow(ctx, "SELECT status FROM discussions WHERE id = $1", id.Param()).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return writeError(w, http.StatusNotFound, "NOT_FOUND", "Discussion not found")
	}
	if err != nil {
		return err
	}
	if status != "waiting" && status != "active" {
		return httpapi.BadRequest(w, "Discussion is "+status+" and not accepting deferrals")
	}
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return err
	}
	var pStatus string
	var deferCount *int
	err = s.DB.QueryRow(ctx,
		"SELECT status, defer_count FROM discussion_participants WHERE discussion_id = $1 AND actor_id = $2",
		id.Param(), actor.ID).Scan(&pStatus, &deferCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return writeError(w, http.StatusNotFound, "NOT_FOUND", "Agent is not a participant in this discussion")
	}
	if err != nil {
		return err
	}
	if pStatus != "invited" && pStatus != "deferred" {
		return httpapi.BadRequest(w, "Can only defer from invited or deferred status, current status: "+pStatus)
	}
	maxText, err := s.Config.Get("max_defer_count")
	if err != nil {
		return err
	}
	// parseInt: a value it cannot read is NaN, which no count reaches.
	maxDefers, maxOK := jsutil.ParseInt(maxText)
	count := 0
	if deferCount != nil {
		count = *deferCount
	}
	if maxOK && float64(count) >= maxDefers {
		return writeError(w, http.StatusBadRequest, "MAX_DEFERRALS_REACHED",
			"Maximum deferrals reached ("+jsutil.NumberString(maxDefers)+"). Must join or let timeout expire.")
	}

	if _, err := s.DB.Exec(ctx,
		"UPDATE discussion_participants SET status = $1, deferred_at = NOW(), defer_count = defer_count + 1 WHERE discussion_id = $2 AND actor_id = $3",
		"deferred", id.Param(), actor.ID); err != nil {
		return err
	}
	timeoutText, err := s.Config.Get("discussion_defer_timeout")
	if err != nil {
		return err
	}
	timeoutAt, timeoutParam, _ := minutesFromNow(s.now(), timeoutText)
	if _, err := s.DB.Exec(ctx, "UPDATE discussions SET timeout_at = $1 WHERE id = $2", timeoutParam, id.Param()); err != nil {
		return err
	}
	var deferredAt *time.Time
	var newCount *int
	if err := s.DB.QueryRow(ctx,
		"SELECT deferred_at, defer_count FROM discussion_participants WHERE discussion_id = $1 AND actor_id = $2",
		id.Param(), actor.ID).Scan(&deferredAt, &newCount); err != nil {
		return err
	}
	timeoutISO := jsutil.DateISO(timeoutAt)
	s.log("defer", logger.Fields{field("discussion_id", id), field("agent", agent), field("defer_count", newCount), field("timeout_at", timeoutISO)})
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		DiscussionID ID      `json:"discussion_id"`
		Agent        string  `json:"agent"`
		DeferredAt   *string `json:"deferred_at"`
		DeferCount   *int    `json:"defer_count"`
		TimeoutAt    string  `json:"timeout_at"`
	}{id, agent, isoTime(deferredAt), newCount, timeoutISO})
}

func (rt *Routes) leave(w http.ResponseWriter, r *http.Request) error {
	agent, id, done, err := agentAndDiscussion(w, r)
	if done || err != nil {
		return err
	}
	ctx, cancel := detached(r)
	defer cancel()
	s := rt.Service

	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return err
	}
	_, ok, err := s.participantStatus(ctx, id.Param(), actor.ID)
	if err != nil {
		return err
	}
	if !ok {
		return writeError(w, http.StatusNotFound, "NOT_FOUND", "Agent is not a participant in this discussion")
	}
	if _, err := s.DB.Exec(ctx,
		"UPDATE discussion_participants SET status = $1 WHERE discussion_id = $2 AND actor_id = $3",
		"left", id.Param(), actor.ID); err != nil {
		return err
	}
	s.log("leave", logger.Fields{field("discussion_id", id), field("agent", agent)})
	s.sendEvent(ctx, id, agent+" left the discussion", "Failed to send leave event")
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		DiscussionID ID     `json:"discussion_id"`
		Agent        string `json:"agent"`
		Status       string `json:"status"`
	}{id, agent, "left"})
}

type proposedVote struct {
	ID           int     `json:"id"`
	DiscussionID ID      `json:"discussion_id"`
	ProposedBy   string  `json:"proposed_by"`
	Question     string  `json:"question"`
	Type         string  `json:"type"`
	Threshold    string  `json:"threshold"`
	ClosesAt     ID      `json:"closes_at"`
	CreatedAt    *string `json:"created_at"`
}

// voteKind is `x || fallback` checked against the allowed values; ok is
// false for anything else (a value of another JSON type included).
func voteKind(f httpapi.Fields, name, fallback string, allowed ...string) (string, bool) {
	if !f.Truthy(name) {
		return fallback, true
	}
	s, isString := f.String(name)
	return s, isString && contains(allowed, s)
}

func (rt *Routes) voteProposeRoute(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("discussion_id")
	}
	ctx, cancel := detached(r)
	defer cancel()
	s := rt.Service

	proposedBy, done, err := httpapi.Identity(w, r, f, "proposed_by", "proposed_by does not match authenticated agent")
	if done || err != nil {
		return err
	}
	id := RawID(f.Raw("discussion_id"))
	question, questionIsString, questionTruthy := contentField(f, "question")
	if !id.Truthy() || proposedBy == "" || !questionTruthy {
		return httpapi.BadRequest(w, "Required fields: discussion_id, proposed_by, question")
	}
	if !questionIsString {
		return httpapi.NotString("question")
	}

	// The route's own requireJoined: an unknown proposer is a 404.
	proposer, err := s.Actors.RequireByName(ctx, proposedBy)
	if err != nil {
		return err
	}
	refused, err := s.joinedActor(ctx, id.Param(), proposer.ID)
	if err != nil {
		return err
	}
	if refused != nil {
		return writeError(w, http.StatusForbidden, refused.code, refused.message)
	}
	var status string
	err = s.DB.QueryRow(ctx, "SELECT status FROM discussions WHERE id = $1", id.Param()).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("Cannot read properties of undefined (reading 'status')")
	}
	if err != nil {
		return err
	}
	if status != "active" && status != "waiting" {
		return httpapi.BadRequest(w, "Discussion is not active or waiting")
	}
	voteType, ok := voteKind(f, "type", "general", "general", "conclude")
	if !ok {
		return httpapi.BadRequest(w, `Vote type must be "general" or "conclude"`)
	}
	threshold, ok := voteKind(f, "threshold", "unanimous", "unanimous", "majority")
	if !ok {
		return httpapi.BadRequest(w, `Threshold must be "unanimous" or "majority"`)
	}
	closesAt := RawID(f.Raw("closes_at"))
	var closesParam any
	if closesAt.Truthy() {
		closesParam = closesAt.Param()
	} else {
		closesAt = RawID(json.RawMessage("null"))
	}

	if proposer, err = s.Actors.RequireByName(ctx, proposedBy); err != nil {
		return err
	}
	var voteID int
	var createdAt time.Time
	if err := s.DB.QueryRow(ctx,
		"INSERT INTO discussion_votes (discussion_id, proposed_by_actor_id, question, type, threshold, closes_at) VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at",
		id.Param(), proposer.ID, question, voteType, threshold, closesParam).Scan(&voteID, &createdAt); err != nil {
		return err
	}
	s.log("vote_propose", logger.Fields{field("discussion_id", id), field("vote_id", voteID), field("proposed_by", proposedBy), field("question", question), field("type", voteType)})
	s.sendEvent(ctx, id, proposedBy+" proposed "+voteType+" vote #"+strconv.Itoa(voteID)+": "+question, "Failed to send vote propose event")

	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Vote proposedVote `json:"vote"`
	}{proposedVote{
		ID: voteID, DiscussionID: id, ProposedBy: proposedBy, Question: question, Type: voteType,
		Threshold: threshold, ClosesAt: closesAt, CreatedAt: isoTime(&createdAt),
	}})
}

func (rt *Routes) voteCast(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return noBodyRead("agent")
	}
	ctx, cancel := detached(r)
	defer cancel()
	agent, done, err := agentIdentity(w, r, f)
	if done || err != nil {
		return err
	}
	reason := f.Raw("reason")
	if s, ok := f.String("reason"); ok {
		reason, _ = json.Marshal(sanitize.Content(s))
	}
	res, err := rt.Service.VoteCast(ctx, RawID(f.Raw("vote_id")), agent, f.Raw("choice"), reason)
	if err != nil {
		return err
	}
	return httpapi.WriteJSON(w, http.StatusOK, res)
}

func (rt *Routes) voteStatus(w http.ResponseWriter, r *http.Request) error {
	f, ok := httpapi.BodyFields(r)
	if !ok {
		return httpapi.NoBody("vote_id")
	}
	ctx, cancel := detached(r)
	defer cancel()
	s := rt.Service

	voteID := RawID(f.Raw("vote_id"))
	if !voteID.Truthy() {
		return httpapi.BadRequest(w, "Required field: vote_id")
	}
	var status string
	var discussionID int
	err := s.DB.QueryRow(ctx, "SELECT status, discussion_id FROM discussion_votes WHERE id = $1", voteID.Param()).Scan(&status, &discussionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return writeError(w, http.StatusNotFound, "NOT_FOUND", "Vote not found")
	}
	if err != nil {
		return err
	}
	if agent := httpapi.StateOf(r).AuthenticatedAgent; agent != "" {
		ok, err := s.participantOf(ctx, discussionID, agent)
		if err != nil {
			return err
		}
		if !ok {
			return writeError(w, http.StatusForbidden, "FORBIDDEN", "Not a participant in this discussion")
		}
	}
	if status == "open" {
		if err := s.EvaluateVote(ctx, voteID); err != nil {
			return err
		}
	}
	votes, err := queryVotes(ctx, s.DB, "SELECT "+voteColumns+`, ac.name AS proposed_by
         FROM discussion_votes v
         JOIN actors ac ON ac.id = v.proposed_by_actor_id
         WHERE v.id = $1`, voteID.Param())
	if err != nil {
		return err
	}
	ballots, err := queryBallots(ctx, s.DB, voteID.Param())
	if err != nil {
		return err
	}
	s.log("vote_status", logger.Fields{field("vote_id", voteID)})
	var vote *Vote
	if len(votes) > 0 {
		vote = &votes[0]
	}
	return httpapi.WriteJSON(w, http.StatusOK, struct {
		Vote    *Vote    `json:"vote,omitempty"`
		Ballots []Ballot `json:"ballots"`
	}{vote, ballots})
}
