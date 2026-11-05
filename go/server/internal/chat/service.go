// Package chat is Node's services/chat.js and routes/chat.js: chat between
// agents, in discussions, and between the salem engine and its NPC virtual
// agents. The service takes names and decoded values, not requests, because
// the MCP tools, the admin routes and the virtual agents call it too.
package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/events"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgbind"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/system"
)

// DirectChat is one chat message to a virtual agent, as Node passed it to
// handleDirectChat.
type DirectChat struct {
	VirtualAgent string
	FromAgent    string
	// Message is nil for a tool-results message, which has no text.
	Message *string
	// MessageID is the recipient's delivery row.
	MessageID int
	// ToolsOffered is the raw JSON array, nil when none was sent.
	ToolsOffered     json.RawMessage
	IsToolResultCall bool
	SceneID          *string
	SceneStructure   *string
	ConversationID   *string
	SimActorID       *string
	SimActorName     *string
	HistoryScope     *string
	EphemeralContext *string
	StableContext    *string
	// AckReplyOnInsert acks the reply row as it is written: a wait=true
	// caller reads the reply in the response and never acks it.
	AckReplyOnInsert bool
	// NotifyUnanswerable sends the "[Error] cannot reply" notice back when
	// the agent will not reply.
	NotifyUnanswerable bool
}

// VirtualAgents is what chat needs from the virtual agents: the notice sent
// back when a sender may not use one, and the reply to a direct message. The
// reply is the JSON the wait=true route returns as "reply"; nil is null.
type VirtualAgents interface {
	UnanswerableNotice(agentName, reason string, missing []string) string
	HandleDirectChat(ctx context.Context, in DirectChat) (json.RawMessage, error)
}

// RateLimitedError is the reply failure a wait=true caller gets as 429
// RATE_LIMITED, with resumes_in_seconds when set.
type RateLimitedError struct {
	Message          string
	ResumesInSeconds *float64
}

func (e *RateLimitedError) Error() string { return e.Message }

// OverBudgetError is the reply failure a wait=true caller gets as 402
// OVER_BUDGET.
type OverBudgetError struct {
	Message string
}

func (e *OverBudgetError) Error() string { return e.Message }

// Service is the chat service.
type Service struct {
	DB       db.DB
	Actors   *actors.Cache
	Log      *logger.Logger
	Events   events.Broadcaster
	Agents   VirtualAgents
	System   *system.Handler
	Notifier *system.Notifier
}

func (s *Service) logChat(action string, details logger.Fields) {
	s.Log.Log("chat", action, details)
}

func badRequest(message string) error {
	return apierr.New(http.StatusBadRequest, message)
}

// Time is a timestamptz in a response, as node-postgres and JSON.stringify
// wrote it; NULL is null.
type Time = *string

func jsTime(t *time.Time) Time {
	if t == nil {
		return nil
	}
	s := jsutil.DateISO(*t)
	return &s
}

// JSON is a jsonb column in a response: node-postgres parsed it and
// JSON.stringify wrote it again. NULL is null.
type JSON []byte

// MarshalJSON writes the value as JSON.stringify would.
func (j JSON) MarshalJSON() ([]byte, error) {
	if j == nil {
		return []byte("null"), nil
	}
	s, err := jsutil.Stringify(j)
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

const maxSafeInteger = 1<<53 - 1

// safeInt is util.js safeInt: a JSON number, or a string that is not blank,
// whose Number() is a safe integer.
func safeInt(raw json.RawMessage) (int64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0, false
	}
	var n float64
	switch c := raw[0]; {
	case c == '"':
		var s string
		if json.Unmarshal(raw, &s) != nil || jsutil.Trim(s) == "" {
			return 0, false
		}
		n = jsutil.StringToNumber(s)
	case c == '-' || c >= '0' && c <= '9':
		n = jsutil.ToNumber(raw)
	default:
		return 0, false
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) || math.Abs(n) > maxSafeInteger {
		return 0, false
	}
	return int64(n), true
}

var discussionChannel = regexp.MustCompile(`^discussion-(\d+)$`)

// ResolveDiscussionID reads discussion_id, or the legacy channel
// "discussion-<N>" when discussion_id is absent, null or "". The result is
// a positive integer or 0 (none). A truthy channel that is not a string is
// refused; Node's channel.match threw a TypeError for it.
func ResolveDiscussionID(discussionID, channel json.RawMessage) (int64, error) {
	if present(discussionID) && string(bytes.TrimSpace(discussionID)) != `""` {
		if n, ok := safeInt(discussionID); ok && n > 0 {
			return n, nil
		}
		return 0, nil
	}
	if !pgbind.Truthy(channel) {
		return 0, nil
	}
	var s string
	trimmed := bytes.TrimSpace(channel)
	if trimmed[0] != '"' || json.Unmarshal(trimmed, &s) != nil {
		return 0, apierr.New(http.StatusBadRequest, "channel must be a string")
	}
	m := discussionChannel.FindStringSubmatch(s)
	if m == nil {
		return 0, nil
	}
	if n, ok := safeInt(json.RawMessage(`"` + m[1] + `"`)); ok && n > 0 {
		return n, nil
	}
	return 0, nil
}

// present is `x !== undefined && x !== null` for a raw body value.
func present(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && string(raw) != "null"
}

// discussionParam is a discussion id as node-postgres bound the JS number,
// so Postgres refuses one past int4 with its own message.
func discussionParam(id int64) any {
	if id == 0 {
		return nil
	}
	return jsutil.Num(float64(id))
}

// ToAgents is chatSend's to_agents argument.
type ToAgents struct {
	// Given is false when to_agents was falsy (absent, null, "", 0, false).
	Given bool
	// IsArray is false for a truthy value that is not an array.
	IsArray bool
	// Names are the array's names, in order. ["*"] is everyone with an
	// agent configuration; the string "*" is also given as ["*"], since
	// Node read it the same way (length 1, first character "*").
	Names []string
}

// To is a to_agents array of names.
func To(names ...string) ToAgents {
	return ToAgents{Given: true, IsArray: true, Names: names}
}

func (t ToAgents) everyone() bool {
	return t.Given && len(t.Names) == 1 && t.Names[0] == "*"
}

// ToolCallResult is one tool's result in a tool_call_results message.
type ToolCallResult struct {
	ID      string
	Content string
}

// SendInput are chatSend's arguments and options.
type SendInput struct {
	FromAgent    string
	ToAgents     ToAgents
	DiscussionID int64
	// Message nil is undefined or null in Node.
	Message *string

	// ToolCalls and ToolsOffered are raw JSON arrays; nil is none.
	ToolCalls    json.RawMessage
	ToolsOffered json.RawMessage
	ToolCallID   *string
	// ToolCallResults, when not empty, replaces Message with one row per
	// result.
	ToolCallResults []ToolCallResult
	// PersistOnly stores the rows and dispatches no virtual-agent call.
	PersistOnly bool

	SceneID          *string
	SceneStructure   *string
	ConversationID   *string
	SimActorID       *string
	SimActorName     *string
	HistoryScope     *string
	EphemeralContext *string
	StableContext    *string

	// IsError marks retry and error rows that history readers skip.
	IsError bool
	// Wait means the caller waits for the virtual agent's reply (Pending).
	Wait bool
	// AckOnInsert acks the delivery rows as they are written.
	AckOnInsert bool
}

// Delivery is one recipient's row in a send result.
type Delivery struct {
	ID     int    `json:"id"`
	Agent  string `json:"agent"`
	SentAt Time   `json:"sent_at"`
}

// Sent is the answer to a send.
type Sent struct {
	FromAgent string     `json:"from_agent"`
	ToAgents  []Delivery `json:"to_agents"`
	SentAt    Time       `json:"sent_at"`
}

// postCommitTimeout bounds the virtual-agent check after a send commits,
// like db.Background's bound on fire-and-forget work.
const postCommitTimeout = 30 * time.Second

// PendingReply runs the virtual agent's reply for a wait=true send.
type PendingReply func(ctx context.Context) (json.RawMessage, error)

// rowSpec is one text row to insert: the message, or one tool result.
type rowSpec struct {
	message    *string
	toolCallID *string
}

type insertedText struct {
	id      int
	sentAt  time.Time
	message *string
}

type chatEvent struct {
	ID             int      `json:"id"`
	MessageTextID  int      `json:"message_text_id"`
	FromAgent      string   `json:"from_agent"`
	ToAgents       []string `json:"to_agents"`
	Message        *string  `json:"message,omitempty"`
	DiscussionID   *int64   `json:"discussion_id"`
	SceneID        *string  `json:"scene_id"`
	ConversationID *string  `json:"conversation_id"`
	SentAt         Time     `json:"sent_at"`
}

type notifyPayload struct {
	Type         string `json:"type"`
	DiscussionID int64  `json:"discussionId"`
	TriggerType  string `json:"triggerType"`
}

// Send stores a chat message for its recipients: the discussion's joined
// participants, the named agents, or everyone ("*"), less the sender. The
// text rows and delivery rows commit together. Then, after the commit: a
// live event per text row, the system handler for a message to "system",
// the discussion's virtual-agent trigger, and the direct-chat dispatch to
// any virtual recipient. pending is set only for a Wait send with exactly
// one virtual recipient the sender may use.
func (s *Service) Send(ctx context.Context, in SendInput) (sent *Sent, pending PendingReply, err error) {
	results := in.ToolCallResults
	if in.FromAgent == "" {
		return nil, nil, badRequest("Required field: from_agent")
	}
	if len(results) == 0 && in.Message == nil {
		return nil, nil, badRequest("Required field: message")
	}
	if !in.ToAgents.Given && in.DiscussionID == 0 {
		return nil, nil, badRequest("Required: to_agents (array) or discussion_id")
	}

	fromActor, err := s.Actors.RequireByName(ctx, in.FromAgent)
	if err != nil {
		return nil, nil, err
	}

	var recipients []string
	seen := map[string]bool{}
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			recipients = append(recipients, name)
		}
	}
	if in.DiscussionID != 0 {
		names, err := s.queryNames(ctx, `SELECT ac.name AS agent FROM discussion_participants dp
             JOIN actors ac ON ac.id = dp.actor_id
             WHERE dp.discussion_id = $1 AND dp.status = $2 AND dp.actor_id != $3`,
			discussionParam(in.DiscussionID), "joined", fromActor.ID)
		if err != nil {
			return nil, nil, err
		}
		for _, n := range names {
			add(n)
		}
	}
	if in.ToAgents.everyone() {
		names, err := s.queryNames(ctx, `SELECT ac.name AS agent FROM agent_configuration agc
             JOIN actors ac ON ac.id = agc.actor_id
             WHERE ac.name != $1`, in.FromAgent)
		if err != nil {
			return nil, nil, err
		}
		for _, n := range names {
			add(n)
		}
	} else if in.ToAgents.Given {
		if !in.ToAgents.IsArray || len(in.ToAgents.Names) == 0 {
			return nil, nil, badRequest("to_agents must be a non-empty array")
		}
		for _, n := range in.ToAgents.Names {
			add(n)
		}
	}
	if seen[in.FromAgent] {
		kept := recipients[:0]
		for _, n := range recipients {
			if n != in.FromAgent {
				kept = append(kept, n)
			}
		}
		recipients = kept
	}
	if len(recipients) == 0 {
		return nil, nil, badRequest("No recipients resolved")
	}

	recipientActors, err := s.Actors.ResolveMultipleByName(ctx, recipients)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range recipients {
		if _, ok := recipientActors[name]; !ok {
			return nil, nil, apierr.New(http.StatusNotFound, fmt.Sprintf("Agent \"%s\" is not registered", name))
		}
	}

	specs := []rowSpec{{in.Message, in.ToolCallID}}
	if len(results) > 0 {
		specs = make([]rowSpec, len(results))
		for i, r := range results {
			content, id := r.Content, r.ID
			specs[i] = rowSpec{&content, &id}
		}
	}

	toolCalls, err := jsonbParam(in.ToolCalls)
	if err != nil {
		return nil, nil, err
	}
	toolsOffered, err := jsonbParam(in.ToolsOffered)
	if err != nil {
		return nil, nil, err
	}

	texts, deliveries, allDeliveryIDs, firstDelivery, err := s.insert(ctx, in, fromActor.ID, recipients, recipientActors, specs, toolCalls, toolsOffered)
	if err != nil {
		return nil, nil, err
	}

	last := texts[len(texts)-1]
	var discussionID *int64
	if in.DiscussionID != 0 {
		id := in.DiscussionID
		discussionID = &id
	}

	if s.Events != nil {
		for _, t := range texts {
			s.Events.Broadcast("chat_message", chatEvent{
				ID: firstDelivery[t.id], MessageTextID: t.id, FromAgent: in.FromAgent, ToAgents: recipients,
				Message: t.message, DiscussionID: discussionID, SceneID: in.SceneID, ConversationID: in.ConversationID,
				SentAt: jsTime(&t.sentAt),
			})
		}
	}

	s.logChat("send", logger.Fields{
		{Key: "from_agent", Value: in.FromAgent}, {Key: "to_agents", Value: recipients}, {Key: "message_text_id", Value: last.id},
		{Key: "delivery_ids", Value: allDeliveryIDs}, {Key: "discussion_id", Value: discussionID},
		{Key: "tool_results_count", Value: len(results)}, {Key: "persist_only", Value: in.PersistOnly},
	})

	for _, d := range deliveries {
		if d.Agent == "system" && s.System != nil {
			id, from, message := d.ID, in.FromAgent, in.Message
			db.Background(func(ctx context.Context) {
				s.System.HandleMessage(ctx, id, from, message, nil)
			})
			break
		}
	}

	if in.DiscussionID != 0 && in.FromAgent != "system" && s.Notifier != nil {
		payload := notifyPayload{Type: "virtual-agent", DiscussionID: in.DiscussionID, TriggerType: "message"}
		db.Background(func(ctx context.Context) {
			s.Notifier.NotifySystem(ctx, payload)
		})
	}

	if !in.PersistOnly && in.DiscussionID == 0 && in.FromAgent != "system" {
		// Best effort, as in Node: a failure here never fails the send. The
		// message is committed, so the check and the notice go on if the
		// client hangs up, as Node's did.
		postCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), postCommitTimeout)
		pending, _ = s.dispatchVirtualAgents(postCtx, in, fromActor, recipients, recipientActors, deliveries)
		cancel()
	}

	return &Sent{FromAgent: in.FromAgent, ToAgents: deliveries, SentAt: jsTime(&last.sentAt)}, pending, nil
}

// jsonbParam is JSON.stringify of a JSON value bound for a jsonb column;
// nil is NULL.
func jsonbParam(raw json.RawMessage) (*string, error) {
	if !present(raw) {
		return nil, nil
	}
	// Node kept a lone surrogate and Postgres refused the jsonb, a 500; Go's
	// decoder would store U+FFFD instead.
	if jsutil.HasLoneSurrogateEscape(raw) {
		return nil, errors.New("tool data holds a lone UTF-16 surrogate, which jsonb cannot store")
	}
	s, err := jsutil.Stringify(raw)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// insert writes the text rows, then one delivery row per text row and
// recipient, in one transaction. A delivery's id in the answer is the
// recipient's last row; firstDelivery maps each text row to its first
// recipient's row, which its live event names.
func (s *Service) insert(ctx context.Context, in SendInput, fromActorID int, recipients []string, recipientActors map[string]actors.Actor,
	specs []rowSpec, toolCalls, toolsOffered *string) (texts []insertedText, deliveries []Delivery, allIDs []int, firstDelivery map[int]int, err error) {
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer tx.Rollback(ctx)

	for i, spec := range specs {
		var calls, offered *string
		if i == 0 {
			calls, offered = toolCalls, toolsOffered
		}
		var t insertedText
		if err := tx.QueryRow(ctx, `INSERT INTO chat_message_texts
                (message, from_actor_id, discussion_id, sent_at, tool_calls, tool_call_id, tools_offered, scene_id, scene_structure, is_error, conversation_id)
             VALUES (
                $1, $2, $3, NOW(), $4, $5, $6, $7,
                COALESCE($8, CASE
                    WHEN $7::uuid IS NULL THEN NULL
                    ELSE (
                        SELECT scene_structure FROM chat_message_texts
                         WHERE scene_id = $7::uuid AND scene_structure IS NOT NULL
                         ORDER BY id ASC LIMIT 1
                    )
                END),
                $9, $10
             )
             RETURNING id, sent_at`,
			spec.message, fromActorID, discussionParam(in.DiscussionID), calls, spec.toolCallID, offered,
			in.SceneID, in.SceneStructure, in.IsError, in.ConversationID).Scan(&t.id, &t.sentAt); err != nil {
			return nil, nil, nil, nil, err
		}
		t.message = spec.message
		texts = append(texts, t)
	}

	firstDelivery = map[int]int{}
	allIDs = []int{}
	sentAt := jsTime(&texts[len(texts)-1].sentAt)
	for _, name := range recipients {
		toActor := recipientActors[name]
		last := 0
		for _, t := range texts {
			if err := tx.QueryRow(ctx, `INSERT INTO chat_messages (message_text_id, to_actor_id, acked_at)
                 VALUES ($1, $2, CASE WHEN $3::boolean THEN NOW() ELSE NULL END)
                 RETURNING id`, t.id, toActor.ID, in.AckOnInsert).Scan(&last); err != nil {
				return nil, nil, nil, nil, err
			}
			allIDs = append(allIDs, last)
			if _, ok := firstDelivery[t.id]; !ok {
				firstDelivery[t.id] = last
			}
		}
		deliveries = append(deliveries, Delivery{ID: last, Agent: name, SentAt: sentAt})
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, nil, nil, err
	}
	return texts, deliveries, allIDs, firstDelivery, nil
}

// dispatchVirtualAgents hands a direct message to each virtual recipient the
// sender may use. A sender without access gets the unanswerable notice back
// from that agent (LLM-669), but only for a non-wait message to exactly one
// named agent: a "*" broadcast would flood the sender, and a wait=true
// caller gets the failure as its answer. One dispatch is returned as pending
// for a Wait send; any other dispatch runs in the background.
func (s *Service) dispatchVirtualAgents(ctx context.Context, in SendInput, fromActor actors.Actor, recipients []string,
	recipientActors map[string]actors.Actor, deliveries []Delivery) (PendingReply, error) {
	senderVirtual, err := s.isVirtual(ctx, fromActor.ID)
	if err != nil || senderVirtual {
		return nil, err
	}
	ids := make([]int, len(recipients))
	for i, name := range recipients {
		ids[i] = recipientActors[name].ID
	}
	virtual, err := s.queryNames(ctx, `SELECT ac.name AS agent FROM agent_configuration agc
         JOIN actors ac ON ac.id = agc.actor_id
         WHERE agc.actor_id = ANY($1) AND agc.virtual = true`, ids)
	if err != nil || len(virtual) == 0 {
		return nil, err
	}

	t := in.ToAgents
	notify := !in.Wait && t.IsArray && len(t.Names) == 1 && t.Names[0] != "*"
	var dispatches []DirectChat
	for _, name := range virtual {
		vaActor := recipientActors[name]
		hasAccess, err := actors.CanAccessVirtualAgent(ctx, s.DB, fromActor.ID, vaActor.ID)
		if err != nil {
			return nil, err
		}
		if !hasAccess {
			if notify {
				notice := s.Agents.UnanswerableNotice(name, "no-access", nil)
				s.Send(ctx, SendInput{FromAgent: name, ToAgents: To(in.FromAgent), Message: &notice, IsError: true})
			}
			continue
		}
		msgID := 0
		for _, d := range deliveries {
			if d.Agent == name {
				msgID = d.ID
				break
			}
		}
		dispatches = append(dispatches, DirectChat{
			VirtualAgent: name, FromAgent: in.FromAgent, Message: in.Message, MessageID: msgID,
			ToolsOffered: in.ToolsOffered, IsToolResultCall: in.ToolCallID != nil && *in.ToolCallID != "" || len(in.ToolCallResults) > 0,
			SceneID: in.SceneID, SceneStructure: in.SceneStructure, ConversationID: in.ConversationID,
			SimActorID: in.SimActorID, SimActorName: in.SimActorName, HistoryScope: in.HistoryScope,
			EphemeralContext: in.EphemeralContext, StableContext: in.StableContext,
		})
	}

	if len(dispatches) == 1 {
		d := dispatches[0]
		d.AckReplyOnInsert = in.Wait
		d.NotifyUnanswerable = notify
		if in.Wait {
			return func(ctx context.Context) (json.RawMessage, error) {
				return s.Agents.HandleDirectChat(ctx, d)
			}, nil
		}
		db.Background(func(ctx context.Context) {
			s.Agents.HandleDirectChat(ctx, d)
		})
		return nil, nil
	}
	for _, d := range dispatches {
		db.Background(func(ctx context.Context) {
			s.Agents.HandleDirectChat(ctx, d)
		})
	}
	return nil, nil
}

// isVirtual is agent_configuration.virtual; no row, NULL and false are not
// virtual.
func (s *Service) isVirtual(ctx context.Context, actorID int) (bool, error) {
	rows, err := s.DB.Query(ctx, "SELECT agc.virtual FROM agent_configuration agc WHERE agc.actor_id = $1", actorID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var virtual *bool
	if rows.Next() {
		if err := rows.Scan(&virtual); err != nil {
			return false, err
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return virtual != nil && *virtual, nil
}

func (s *Service) queryNames(ctx context.Context, sql string, args ...any) ([]string, error) {
	rows, err := s.DB.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// Message is one received message.
type Message struct {
	ID           int     `json:"id"`
	FromAgent    string  `json:"from_agent"`
	ToAgent      string  `json:"to_agent"`
	Message      string  `json:"message"`
	SentAt       Time    `json:"sent_at"`
	ToolCalls    JSON    `json:"tool_calls"`
	ToolCallID   *string `json:"tool_call_id"`
	ToolsOffered JSON    `json:"tools_offered"`
}

// Received is the answer to a receive.
type Received struct {
	Messages     []Message `json:"messages"`
	PendingCount int       `json:"pending_count"`
}

// Receive lists the agent's unacked messages, oldest first: in one
// discussion, or outside every discussion when discussionID is 0. fromAgent
// "" is any sender; afterID nil is no lower bound.
func (s *Service) Receive(ctx context.Context, agent string, discussionID int64, afterID *float64, fromAgent string) (*Received, error) {
	if agent == "" {
		return nil, badRequest("Required field: agent")
	}
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return nil, err
	}
	sql := `SELECT cm.id, fa.name AS from_agent, ta.name AS to_agent, cmt.message, cmt.sent_at,
                        cmt.tool_calls, cmt.tool_call_id, cmt.tools_offered
                 FROM chat_messages cm
                 JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
                 JOIN actors fa ON fa.id = cmt.from_actor_id
                 JOIN actors ta ON ta.id = cm.to_actor_id
                 WHERE cm.to_actor_id = $1 AND cm.acked_at IS NULL AND cm.deleted_at IS NULL`
	params := []any{actor.ID}
	if discussionID != 0 {
		params = append(params, discussionParam(discussionID))
		sql += fmt.Sprintf(" AND cmt.discussion_id = $%d", len(params))
	} else {
		sql += " AND cmt.discussion_id IS NULL"
	}
	if fromAgent != "" {
		from, err := s.Actors.RequireByName(ctx, fromAgent)
		if err != nil {
			return nil, err
		}
		params = append(params, from.ID)
		sql += fmt.Sprintf(" AND cmt.from_actor_id = $%d", len(params))
	}
	if afterID != nil {
		params = append(params, jsutil.Num(*afterID))
		sql += fmt.Sprintf(" AND cm.id > $%d", len(params))
	}
	sql += " ORDER BY cm.id ASC"

	rows, err := s.DB.Query(ctx, sql, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	ids := []int{}
	for rows.Next() {
		var m Message
		var sentAt time.Time
		var calls, offered []byte
		if err := rows.Scan(&m.ID, &m.FromAgent, &m.ToAgent, &m.Message, &sentAt, &calls, &m.ToolCallID, &offered); err != nil {
			return nil, err
		}
		m.SentAt, m.ToolCalls, m.ToolsOffered = jsTime(&sentAt), calls, offered
		out = append(out, m)
		ids = append(ids, m.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var logDiscussion, logAfter any
	if discussionID != 0 {
		logDiscussion = discussionID
	}
	if afterID != nil && *afterID != 0 {
		logAfter = *afterID
	}
	s.logChat("receive", logger.Fields{{Key: "agent", Value: agent}, {Key: "discussion_id", Value: logDiscussion}, {Key: "after_id", Value: logAfter},
		{Key: "pending_count", Value: len(out)}, {Key: "message_ids", Value: ids}})
	return &Received{Messages: out, PendingCount: len(out)}, nil
}

// Acked is the answer to an ack.
type Acked struct {
	Agent    string `json:"agent"`
	Acked    int    `json:"acked"`
	AckedIDs []int  `json:"acked_ids"`
}

// Ack marks the agent's messages read. ids is the caller's raw JSON value, a
// non-empty array; its elements are not checked, so a bad one is Postgres's
// error, as in Node.
func (s *Service) Ack(ctx context.Context, agent string, ids json.RawMessage) (*Acked, error) {
	list, isArray := elements(ids)
	if agent == "" || !isArray || len(list) == 0 {
		return nil, badRequest("Required fields: agent, message_ids (non-empty array)")
	}
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, "UPDATE chat_messages SET acked_at = NOW() WHERE id = ANY($1) AND to_actor_id = $2 AND acked_at IS NULL RETURNING id",
		pgbind.ArrayLiteral(list), actor.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	acked := []int{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		acked = append(acked, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	s.logChat("ack", logger.Fields{{Key: "agent", Value: agent}, {Key: "requested_ids", Value: json.RawMessage(bytes.TrimSpace(ids))}, {Key: "acked_ids", Value: acked}})
	return &Acked{Agent: agent, Acked: len(acked), AckedIDs: acked}, nil
}

// elements returns a JSON array's elements; ok is false for any other value.
func elements(raw json.RawMessage) ([]json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}
	return out, true
}

// Status is the answer to a status call.
type Status struct {
	Agent         string `json:"agent"`
	PendingCount  int64  `json:"pending_count"`
	MaxMessageID  *int   `json:"max_message_id"`
	LastMessageAt Time   `json:"last_message_at"`
	LastAckAt     Time   `json:"last_ack_at"`
}

// Status summarizes the agent's chat in one discussion, or outside every
// discussion when discussionID is 0.
func (s *Service) Status(ctx context.Context, agent string, discussionID int64) (*Status, error) {
	if agent == "" {
		return nil, badRequest("Required field: agent")
	}
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return nil, err
	}
	filter := "AND cmt.discussion_id IS NULL"
	params := []any{actor.ID}
	if discussionID != 0 {
		filter = "AND cmt.discussion_id = $2"
		params = append(params, discussionParam(discussionID))
	}
	const from = `FROM chat_messages cm
         JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
         WHERE cm.to_actor_id = $1`

	out := Status{Agent: agent}
	if err := s.DB.QueryRow(ctx, `SELECT COUNT(*) as count `+from+` AND cm.acked_at IS NULL AND cm.deleted_at IS NULL `+filter, params...).Scan(&out.PendingCount); err != nil {
		return nil, err
	}
	if err := s.DB.QueryRow(ctx, `SELECT MAX(cm.id) as max_id `+from+` AND cm.deleted_at IS NULL `+filter, params...).Scan(&out.MaxMessageID); err != nil {
		return nil, err
	}
	var lastSent, lastAcked *time.Time
	if err := s.DB.QueryRow(ctx, `SELECT MAX(cmt.sent_at) as last_sent `+from+` AND cm.deleted_at IS NULL `+filter, params...).Scan(&lastSent); err != nil {
		return nil, err
	}
	if err := s.DB.QueryRow(ctx, `SELECT MAX(cm.acked_at) as last_acked `+from+` AND cm.acked_at IS NOT NULL AND cm.deleted_at IS NULL `+filter, params...).Scan(&lastAcked); err != nil {
		return nil, err
	}
	out.LastMessageAt, out.LastAckAt = jsTime(lastSent), jsTime(lastAcked)

	var logDiscussion any
	if discussionID != 0 {
		logDiscussion = discussionID
	}
	s.logChat("status", logger.Fields{{Key: "agent", Value: agent}, {Key: "discussion_id", Value: logDiscussion}})
	return &out, nil
}
