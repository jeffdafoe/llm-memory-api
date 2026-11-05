// Package mail is Node's services/mail.js and routes/mail.js: agent-to-agent
// mail. The service takes names and raw JSON values, not requests, because
// the MCP tools, registration, the admin views and the virtual agents call
// it too.
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgbind"
)

// VirtualAgents is what mail needs from the virtual agents: the notice sent
// back when a sender may not use one, and the reply to direct mail.
type VirtualAgents interface {
	UnanswerableNotice(agentName, reason string, missing []string) string
	HandleDirectMail(ctx context.Context, virtualAgent, fromAgent, mailID string) error
}

// Service is the mail service.
type Service struct {
	DB     db.DB
	Actors *actors.Cache
	Log    *logger.Logger
	Agents VirtualAgents
}

func (s *Service) logMail(action string, details logger.Fields) {
	s.Log.Log("mail", action, details)
}

func badRequest(message string) error {
	return apierr.New(http.StatusBadRequest, message)
}

var errNotFound = apierr.New(http.StatusNotFound, "Mail not found, not owned by you, or already acked")

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

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

// Sent is the answer to a send.
type Sent struct {
	ID        string `json:"id"`
	ToAgent   string `json:"to_agent"`
	FromAgent string `json:"from_agent"`
	Subject   string `json:"subject"`
	SentAt    Time   `json:"sent_at"`
}

// Send stores a message from one agent to another. inReplyTo "" is none.
// Unless the sender is "system", a virtual recipient is then triggered in
// the background.
func (s *Service) Send(ctx context.Context, toAgent, fromAgent, subject, body, inReplyTo string) (*Sent, error) {
	if toAgent == "" || fromAgent == "" || subject == "" || body == "" {
		return nil, badRequest("Required fields: to_agent, from_agent, subject, body")
	}
	toActor, err := s.Actors.RequireByName(ctx, toAgent)
	if err != nil {
		return nil, err
	}
	fromActor, err := s.Actors.RequireByName(ctx, fromAgent)
	if err != nil {
		return nil, err
	}

	var row pgx.Row
	if inReplyTo != "" {
		row = s.DB.QueryRow(ctx, "INSERT INTO mail (to_actor_id, from_actor_id, subject, body, in_reply_to) VALUES ($1, $2, $3, $4, $5) RETURNING id, sent_at",
			toActor.ID, fromActor.ID, subject, body, inReplyTo)
	} else {
		row = s.DB.QueryRow(ctx, "INSERT INTO mail (to_actor_id, from_actor_id, subject, body) VALUES ($1, $2, $3, $4) RETURNING id, sent_at",
			toActor.ID, fromActor.ID, subject, body)
	}
	var id string
	var sentAt time.Time
	if err := row.Scan(&id, &sentAt); err != nil {
		return nil, err
	}

	s.logMail("send", logger.Fields{{Key: "from_agent", Value: fromAgent}, {Key: "to_agent", Value: toAgent}, {Key: "mail_id", Value: id}, {Key: "subject", Value: subject}})

	if fromAgent != "system" {
		db.Background(func(ctx context.Context) {
			if err := s.triggerVirtualAgent(ctx, toActor, fromActor, toAgent, fromAgent, subject, id); err != nil {
				s.logMail("virtual-agent-trigger-error", logger.Fields{{Key: "to_agent", Value: toAgent}, {Key: "from_agent", Value: fromAgent}, {Key: "error", Value: err.Error()}})
			}
		})
	}

	return &Sent{ID: id, ToAgent: toAgent, FromAgent: fromAgent, Subject: subject, SentAt: jsTime(&sentAt)}, nil
}

// triggerVirtualAgent answers direct mail to a virtual agent from an agent
// that is not one. A sender without access gets the unanswerable notice,
// sent AS the virtual agent so it reads as that agent's reply (LLM-669); the
// virtual-sender check stops the notice from triggering again.
func (s *Service) triggerVirtualAgent(ctx context.Context, toActor, fromActor actors.Actor, toAgent, fromAgent, subject, mailID string) error {
	recipientVirtual, err := s.isVirtual(ctx, toActor.ID)
	if err != nil {
		return err
	}
	senderVirtual, err := s.isVirtual(ctx, fromActor.ID)
	if err != nil {
		return err
	}
	if !recipientVirtual || senderVirtual {
		return nil
	}
	hasAccess, err := actors.CanAccessVirtualAgent(ctx, s.DB, fromActor.ID, toActor.ID)
	if err != nil {
		return err
	}
	if !hasAccess {
		s.logMail("virtual-agent-access-denied", logger.Fields{{Key: "from_agent", Value: fromAgent}, {Key: "to_agent", Value: toAgent}})
		errSubject := subject
		if !strings.HasPrefix(subject, "Re: ") {
			errSubject = "Re: " + subject
		}
		_, err := s.Send(ctx, fromAgent, toAgent, errSubject, s.Agents.UnanswerableNotice(toAgent, "no-access", nil), mailID)
		return err
	}
	if err := s.Agents.HandleDirectMail(ctx, toAgent, fromAgent, mailID); err != nil {
		s.logMail("virtual-agent-trigger-error", logger.Fields{{Key: "to_agent", Value: toAgent}, {Key: "from_agent", Value: fromAgent}, {Key: "error", Value: err.Error()}})
	}
	return nil
}

// isVirtual is agent_configuration.virtual; no row, NULL and false are not
// virtual.
func (s *Service) isVirtual(ctx context.Context, actorID int) (bool, error) {
	rows, err := s.DB.Query(ctx, "SELECT virtual FROM agent_configuration WHERE actor_id = $1", actorID)
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

// Message is a received message (receive).
type Message struct {
	ID        string  `json:"id"`
	FromAgent string  `json:"from_agent"`
	ToAgent   string  `json:"to_agent"`
	Subject   string  `json:"subject"`
	Body      string  `json:"body"`
	SentAt    Time    `json:"sent_at"`
	InReplyTo *string `json:"in_reply_to"`
}

// Messages is the answer of the list calls.
type Messages[T any] struct {
	Messages []T `json:"messages"`
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

// Receive returns the agent's unacked messages with the given ids. ids is
// the caller's raw JSON value: it must be a non-empty array of UUID strings.
func (s *Service) Receive(ctx context.Context, agent string, ids json.RawMessage) (*Messages[Message], error) {
	if agent == "" {
		return nil, badRequest("Required field: agent")
	}
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return nil, err
	}
	list, isArray := elements(ids)
	if !isArray || len(list) == 0 {
		return nil, badRequest("Required field: ids (array of mail UUIDs). " +
			"Use mail_check first to list unread mail with IDs, " +
			"then call mail_receive with specific IDs to read full content, " +
			"then call mail_ack with those IDs after processing.")
	}
	for _, e := range list {
		var id string
		e = bytes.TrimSpace(e)
		if len(e) == 0 || e[0] != '"' || json.Unmarshal(e, &id) != nil || !uuidPattern.MatchString(id) {
			return nil, badRequest("Invalid UUID in ids array")
		}
	}

	rows, err := s.DB.Query(ctx, `SELECT m.id, fa.name AS from_agent, ta.name AS to_agent, m.subject, m.body, m.sent_at, m.in_reply_to
             FROM mail m
             JOIN actors fa ON fa.id = m.from_actor_id
             JOIN actors ta ON ta.id = m.to_actor_id
             WHERE m.id = ANY($1) AND m.to_actor_id = $2 AND m.acked_at IS NULL AND m.deleted_at IS NULL
             ORDER BY m.sent_at ASC`, pgbind.ArrayLiteral(list), actor.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	messageIDs := []string{}
	for rows.Next() {
		var m Message
		var sentAt time.Time
		if err := rows.Scan(&m.ID, &m.FromAgent, &m.ToAgent, &m.Subject, &m.Body, &sentAt, &m.InReplyTo); err != nil {
			return nil, err
		}
		m.SentAt = jsTime(&sentAt)
		out = append(out, m)
		messageIDs = append(messageIDs, m.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	s.logMail("receive", logger.Fields{{Key: "agent", Value: agent}, {Key: "pending_count", Value: len(out)}, {Key: "message_ids", Value: messageIDs}})
	return &Messages[Message]{out}, nil
}

// Acked is the answer to an ack.
type Acked struct {
	Agent    string   `json:"agent"`
	Acked    int      `json:"acked"`
	AckedIDs []string `json:"acked_ids"`
}

// Ack marks the agent's messages read. ids is the caller's raw JSON value,
// a non-empty array; its elements are not checked, so a bad one is
// Postgres's error, as in Node.
func (s *Service) Ack(ctx context.Context, agent string, ids json.RawMessage) (*Acked, error) {
	list, isArray := elements(ids)
	if agent == "" || !isArray || len(list) == 0 {
		return nil, badRequest("Required fields: agent, message_ids (array of UUIDs)")
	}
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, "UPDATE mail SET acked_at = NOW() WHERE id = ANY($1) AND to_actor_id = $2 AND acked_at IS NULL RETURNING id",
		pgbind.ArrayLiteral(list), actor.ID)
	if err != nil {
		return nil, err
	}
	acked, err := scanIDs(rows)
	if err != nil {
		return nil, err
	}

	s.logMail("ack", logger.Fields{{Key: "agent", Value: agent}, {Key: "requested_ids", Value: json.RawMessage(bytes.TrimSpace(ids))}, {Key: "acked_ids", Value: acked}})
	return &Acked{Agent: agent, Acked: len(acked), AckedIDs: acked}, nil
}

func scanIDs(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}) ([]string, error) {
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Edited is the answer to an edit.
type Edited struct {
	ID      string `json:"id"`
	ToAgent string `json:"to_agent"`
	Subject string `json:"subject"`
	SentAt  Time   `json:"sent_at"`
}

// Edit changes the subject and/or body of a message the sender sent and the
// recipient has not acked. "" leaves a field as it is.
func (s *Service) Edit(ctx context.Context, id, fromAgent, subject, body string) (*Edited, error) {
	if id == "" || fromAgent == "" {
		return nil, badRequest("Required fields: id, from_agent. Optional: subject, body")
	}
	if subject == "" && body == "" {
		return nil, badRequest("At least one of subject or body must be provided")
	}
	fromActor, err := s.Actors.RequireByName(ctx, fromAgent)
	if err != nil {
		return nil, err
	}

	var set []string
	var values []any
	var fields []string
	if subject != "" {
		values = append(values, subject)
		set = append(set, fmt.Sprintf("subject = $%d", len(values)))
		fields = append(fields, "subject")
	}
	if body != "" {
		values = append(values, body)
		set = append(set, fmt.Sprintf("body = $%d", len(values)))
		fields = append(fields, "body")
	}
	values = append(values, id, fromActor.ID)
	sql := fmt.Sprintf(`UPDATE mail SET %s
         WHERE id = $%d AND from_actor_id = $%d AND acked_at IS NULL AND deleted_at IS NULL
         RETURNING id, subject, sent_at`, strings.Join(set, ", "), len(values)-1, len(values))

	var out Edited
	var sentAt time.Time
	if err := s.DB.QueryRow(ctx, sql, values...).Scan(&out.ID, &out.Subject, &sentAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	out.SentAt = jsTime(&sentAt)

	s.logMail("edit", logger.Fields{{Key: "from_agent", Value: fromAgent}, {Key: "mail_id", Value: id}, {Key: "fields", Value: fields}})

	if out.ToAgent, err = s.toAgentOf(ctx, id); err != nil {
		return nil, err
	}
	return &out, nil
}

// toAgentOf is the recipient's name of a message.
func (s *Service) toAgentOf(ctx context.Context, id string) (string, error) {
	var name string
	err := s.DB.QueryRow(ctx, `SELECT ta.name AS to_agent FROM mail m JOIN actors ta ON ta.id = m.to_actor_id WHERE m.id = $1`, id).Scan(&name)
	return name, err
}

// Unsent is the answer to an unsend.
type Unsent struct {
	ID      string `json:"id"`
	ToAgent string `json:"to_agent"`
	Subject string `json:"subject"`
}

// Unsend soft-deletes a message the sender sent and the recipient has not
// acked.
func (s *Service) Unsend(ctx context.Context, id, fromAgent string) (*Unsent, error) {
	if id == "" || fromAgent == "" {
		return nil, badRequest("Required fields: id, from_agent")
	}
	fromActor, err := s.Actors.RequireByName(ctx, fromAgent)
	if err != nil {
		return nil, err
	}
	var out Unsent
	err = s.DB.QueryRow(ctx, `UPDATE mail SET deleted_at = NOW()
         WHERE id = $1 AND from_actor_id = $2 AND acked_at IS NULL AND deleted_at IS NULL
         RETURNING id, subject`, id, fromActor.ID).Scan(&out.ID, &out.Subject)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	if out.ToAgent, err = s.toAgentOf(ctx, id); err != nil {
		return nil, err
	}

	s.logMail("unsend", logger.Fields{{Key: "from_agent", Value: fromAgent}, {Key: "mail_id", Value: id}, {Key: "to_agent", Value: out.ToAgent}, {Key: "subject", Value: out.Subject}})
	return &out, nil
}

// Unread is one line of a check: no full body.
type Unread struct {
	ID          string  `json:"id"`
	FromAgent   string  `json:"from_agent"`
	Subject     string  `json:"subject"`
	SentAt      Time    `json:"sent_at"`
	InReplyTo   *string `json:"in_reply_to"`
	BodyPreview string  `json:"body_preview"`
}

// Check lists the agent's unread mail, oldest first, with a 200-character
// body preview.
func (s *Service) Check(ctx context.Context, agent string) (*Messages[Unread], error) {
	if agent == "" {
		return nil, badRequest("Required field: agent")
	}
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return nil, err
	}
	rows, err := s.DB.Query(ctx, `SELECT m.id, fa.name AS from_agent, m.subject, m.sent_at, m.in_reply_to,
                SUBSTRING(m.body FROM 1 FOR 200) AS body_preview
         FROM mail m
         JOIN actors fa ON fa.id = m.from_actor_id
         WHERE m.to_actor_id = $1 AND m.acked_at IS NULL AND m.deleted_at IS NULL
         ORDER BY m.sent_at ASC`, actor.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Unread{}
	for rows.Next() {
		var m Unread
		var sentAt time.Time
		if err := rows.Scan(&m.ID, &m.FromAgent, &m.Subject, &sentAt, &m.InReplyTo, &m.BodyPreview); err != nil {
			return nil, err
		}
		m.SentAt = jsTime(&sentAt)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	s.logMail("check", logger.Fields{{Key: "agent", Value: agent}, {Key: "pending_count", Value: len(out)}})
	return &Messages[Unread]{out}, nil
}

// ListOptions are the sent and history filters. Peer is the other agent
// ("" for all). Limit and Offset are the caller's raw JSON values.
type ListOptions struct {
	Peer          string
	Limit, Offset json.RawMessage
}

// limitParam is `Math.min(limit || 50, 100)` as node-postgres bound the
// result: Number() rules, so a value that is not a number is NaN, which
// Postgres refuses.
func limitParam(raw json.RawMessage) jsutil.Number {
	n := 50.0
	if pgbind.Truthy(raw) {
		n = jsutil.ToNumber(raw)
	}
	if !math.IsNaN(n) && n > 100 {
		n = 100
	}
	return jsutil.Num(n)
}

// SentMessage is one line of a sent listing.
type SentMessage struct {
	ID          string  `json:"id"`
	ToAgent     string  `json:"to_agent"`
	Subject     string  `json:"subject"`
	BodyPreview string  `json:"body_preview"`
	SentAt      Time    `json:"sent_at"`
	AckedAt     Time    `json:"acked_at"`
	InReplyTo   *string `json:"in_reply_to"`
}

// Sent lists the mail the agent sent (not unsent), newest first, with a
// 200-character body preview. Peer filters by recipient.
func (s *Service) Sent(ctx context.Context, agent string, opts ListOptions) (*Messages[SentMessage], error) {
	if agent == "" {
		return nil, badRequest("Required field: agent")
	}
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return nil, err
	}
	limit, offset := limitParam(opts.Limit), pgbind.OrDefault(opts.Offset, 0)
	conditions := []string{"m.from_actor_id = $1", "m.deleted_at IS NULL"}
	values := []any{actor.ID}
	if opts.Peer != "" {
		peer, err := s.Actors.RequireByName(ctx, opts.Peer)
		if err != nil {
			return nil, err
		}
		values = append(values, peer.ID)
		conditions = append(conditions, fmt.Sprintf("m.to_actor_id = $%d", len(values)))
	}
	values = append(values, limit, offset)

	rows, err := s.DB.Query(ctx, fmt.Sprintf(`SELECT m.id, ta.name AS to_agent, m.subject,
                SUBSTRING(m.body FROM 1 FOR 200) AS body_preview,
                m.sent_at, m.acked_at, m.in_reply_to
         FROM mail m
         JOIN actors ta ON ta.id = m.to_actor_id
         WHERE %s
         ORDER BY m.sent_at DESC
         LIMIT $%d OFFSET $%d`, strings.Join(conditions, " AND "), len(values)-1, len(values)), values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SentMessage{}
	for rows.Next() {
		var m SentMessage
		var sentAt time.Time
		var ackedAt *time.Time
		if err := rows.Scan(&m.ID, &m.ToAgent, &m.Subject, &m.BodyPreview, &sentAt, &ackedAt, &m.InReplyTo); err != nil {
			return nil, err
		}
		m.SentAt, m.AckedAt = jsTime(&sentAt), jsTime(ackedAt)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	s.logMail("sent", logger.Fields{{Key: "agent", Value: agent}, {Key: "count", Value: len(out)}})
	return &Messages[SentMessage]{out}, nil
}

// HistoryMessage is one line of a history listing.
type HistoryMessage struct {
	ID        string  `json:"id"`
	FromAgent string  `json:"from_agent"`
	Subject   string  `json:"subject"`
	Body      string  `json:"body"`
	SentAt    Time    `json:"sent_at"`
	AckedAt   Time    `json:"acked_at"`
	InReplyTo *string `json:"in_reply_to"`
}

// History lists the acked mail the agent received, newest first. Peer
// filters by sender.
func (s *Service) History(ctx context.Context, agent string, opts ListOptions) (*Messages[HistoryMessage], error) {
	if agent == "" {
		return nil, badRequest("Required field: agent")
	}
	actor, err := s.Actors.RequireByName(ctx, agent)
	if err != nil {
		return nil, err
	}
	limit, offset := limitParam(opts.Limit), pgbind.OrDefault(opts.Offset, 0)
	conditions := []string{"m.to_actor_id = $1", "m.acked_at IS NOT NULL", "m.deleted_at IS NULL"}
	values := []any{actor.ID}
	if opts.Peer != "" {
		peer, err := s.Actors.RequireByName(ctx, opts.Peer)
		if err != nil {
			return nil, err
		}
		values = append(values, peer.ID)
		conditions = append(conditions, fmt.Sprintf("m.from_actor_id = $%d", len(values)))
	}
	values = append(values, limit, offset)

	rows, err := s.DB.Query(ctx, fmt.Sprintf(`SELECT m.id, fa.name AS from_agent, m.subject, m.body, m.sent_at, m.acked_at, m.in_reply_to
         FROM mail m
         JOIN actors fa ON fa.id = m.from_actor_id
         WHERE %s
         ORDER BY m.sent_at DESC
         LIMIT $%d OFFSET $%d`, strings.Join(conditions, " AND "), len(values)-1, len(values)), values...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []HistoryMessage{}
	for rows.Next() {
		var m HistoryMessage
		var sentAt time.Time
		var ackedAt *time.Time
		if err := rows.Scan(&m.ID, &m.FromAgent, &m.Subject, &m.Body, &sentAt, &ackedAt, &m.InReplyTo); err != nil {
			return nil, err
		}
		m.SentAt, m.AckedAt = jsTime(&sentAt), jsTime(ackedAt)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	s.logMail("history", logger.Fields{{Key: "agent", Value: agent}, {Key: "count", Value: len(out)}})
	return &Messages[HistoryMessage]{out}, nil
}
