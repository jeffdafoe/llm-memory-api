package mcp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/chat"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/mail"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sanitize"
)

// The chat, mail, agent and activity tools (Node's TOOL_HANDLERS chat_send
// through activity_stop). Like the note tools, each reads validated
// arguments.

// identity is validateIdentity: an identity argument that is given (a
// non-empty string) must be the authenticated agent, which is what the
// tool then acts as.
func identity(args *jsutil.Object, k string, c caller) (string, error) {
	if v, ok := str(args, k); ok && v != "" && v != c.agent {
		return "", forbidden("Identity mismatch: %s \"%s\" does not match authenticated agent \"%s\"", k, v, c.agent)
	}
	return c.agent, nil
}

// maxDiscussionID is 2^63: an int64 carries -2^63 up to, not including, it.
const maxDiscussionID = 9223372036854775808.0

// discussionArg is `args.discussion_id || null` as the chat service takes
// it: 0 for none. Node handed the JavaScript number to Postgres as it was,
// so an integer of any size reaches Postgres here too; a number with a
// fraction, or from 2^63, cannot be carried and is refused.
func discussionArg(args *jsutil.Object) (int64, error) {
	f, ok := jsutil.AsFloat(args.Value("discussion_id"))
	if !ok || f == 0 {
		return 0, nil
	}
	if f != math.Trunc(f) || f < -maxDiscussionID || f >= maxDiscussionID {
		return 0, apierr.New(http.StatusBadRequest, "discussion_id must be an integer")
	}
	return int64(f), nil
}

// preview is a mail listing's body preview line, empty for no preview, with
// "..." when the preview reached 200 by its JavaScript length.
func preview(p string) string {
	if p == "" {
		return ""
	}
	line := "\n> " + p
	if jsutil.UTF16Len(p) >= 200 {
		line += "..."
	}
	return line
}

func (s *Server) chatSend(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	from, err := identity(args, "from", c)
	if err != nil {
		return "", err
	}
	var to chat.ToAgents
	if t, ok := str(args, "to"); ok && t != "" {
		if t == "*" {
			to = chat.To("*")
		} else {
			to = chat.To(sanitize.AgentName(t))
		}
	}
	discussionID, err := discussionArg(args)
	if err != nil {
		return "", err
	}
	message, _ := str(args, "message")
	message = sanitize.Content(message)
	sent, _, err := s.Chat.Send(ctx, chat.SendInput{FromAgent: from, ToAgents: to, DiscussionID: discussionID, Message: &message})
	if err != nil {
		return "", err
	}
	targets := make([]string, len(sent.ToAgents))
	for i, d := range sent.ToAgents {
		targets[i] = d.Agent
	}
	return "Message sent to " + strings.Join(targets, ", "), nil
}

func (s *Server) chatReceive(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	agentName, err := identity(args, "agent", c)
	if err != nil {
		return "", err
	}
	discussionID, err := discussionArg(args)
	if err != nil {
		return "", err
	}
	received, err := s.Chat.Receive(ctx, agentName, discussionID, nil, "")
	if err != nil {
		return "", err
	}
	if len(received.Messages) == 0 {
		return "No new messages.", nil
	}
	lines := make([]string, len(received.Messages))
	ids := make([]string, len(received.Messages))
	for i, m := range received.Messages {
		ids[i] = strconv.Itoa(m.ID)
		lines[i] = fmt.Sprintf("[%s] (id:%d, %s): %s", m.FromAgent, m.ID, s.dateText(m.SentAt), m.Message)
	}
	return strings.Join(lines, "\n") + "\n\n(ids: [" + strings.Join(ids, ", ") + "] — call chat_ack to mark as read)", nil
}

func (s *Server) chatAck(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	acked, err := s.Chat.Ack(ctx, c.agent, raw(args, "ids"))
	if err != nil {
		return "", err
	}
	ids := make([]string, len(acked.AckedIDs))
	for i, id := range acked.AckedIDs {
		ids[i] = strconv.Itoa(id)
	}
	return fmt.Sprintf("Acked %d message(s) for %s: [%s]", acked.Acked, acked.Agent, strings.Join(ids, ", ")), nil
}

func (s *Server) chatStatus(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	agentName, err := identity(args, "agent", c)
	if err != nil {
		return "", err
	}
	discussionID, err := discussionArg(args)
	if err != nil {
		return "", err
	}
	st, err := s.Chat.Status(ctx, agentName, discussionID)
	if err != nil {
		return "", err
	}
	maxID := "null"
	if st.MaxMessageID != nil {
		maxID = strconv.Itoa(*st.MaxMessageID)
	}
	return fmt.Sprintf("Chat status for %s:\n  Pending: %d\n  Max message ID: %s\n  Last message: %s\n  Last ack: %s",
		st.Agent, st.PendingCount, maxID, s.dateText(st.LastMessageAt), s.dateText(st.LastAckAt)), nil
}

func (s *Server) mailSend(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	to, _ := str(args, "to")
	from, err := identity(args, "from", c)
	if err != nil {
		return "", err
	}
	subject, _ := str(args, "subject")
	body, _ := str(args, "body")
	inReplyTo, _ := str(args, "in_reply_to")
	sent, err := s.Mail.Send(ctx, sanitize.AgentName(to), from, sanitize.Content(subject), sanitize.Content(body), inReplyTo)
	if err != nil {
		return "", err
	}
	s.refreshActivity(c)
	return fmt.Sprintf("Mail sent to %s (id: %s, subject: \"%s\")", sent.ToAgent, sent.ID, sent.Subject), nil
}

func (s *Server) mailCheck(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	agentName, err := identity(args, "agent", c)
	if err != nil {
		return "", err
	}
	unread, err := s.Mail.Check(ctx, agentName)
	if err != nil {
		return "", err
	}
	if len(unread.Messages) == 0 {
		return "No unread mail.", nil
	}
	blocks := make([]string, len(unread.Messages))
	for i, m := range unread.Messages {
		line := fmt.Sprintf("**From:** %s | **Date:** %s | **Subject:** %s | **ID:** %s", m.FromAgent, s.dateText(m.SentAt), m.Subject, m.ID)
		if m.InReplyTo != nil && *m.InReplyTo != "" {
			line += " | **In-Reply-To:** " + *m.InReplyTo
		}
		blocks[i] = line + preview(m.BodyPreview)
	}
	return fmt.Sprintf("%d unread message(s):\n\n%s", len(unread.Messages), strings.Join(blocks, "\n\n")), nil
}

// mailBlock is one message of mail_receive and mail_history: the header
// lines, a blank line, the body.
func (s *Server) mailBlock(from string, sentAt *string, subject, id string, inReplyTo *string, body string) string {
	header := fmt.Sprintf("**From:** %s\n**Date:** %s\n**Subject:** %s\n**ID:** %s", from, s.dateText(sentAt), subject, id)
	if inReplyTo != nil && *inReplyTo != "" {
		header += "\n**In-Reply-To:** " + *inReplyTo
	}
	return header + "\n\n" + body
}

func (s *Server) mailReceive(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	agentName, err := identity(args, "agent", c)
	if err != nil {
		return "", err
	}
	received, err := s.Mail.Receive(ctx, agentName, raw(args, "ids"))
	if err != nil {
		return "", err
	}
	if len(received.Messages) == 0 {
		return "No new mail.", nil
	}
	blocks := make([]string, len(received.Messages))
	ids := make([]string, len(received.Messages))
	for i, m := range received.Messages {
		ids[i] = m.ID
		blocks[i] = s.mailBlock(m.FromAgent, m.SentAt, m.Subject, m.ID, m.InReplyTo, m.Body)
	}
	return strings.Join(blocks, "\n\n---\n\n") + "\n\n(ids: [" + strings.Join(ids, ", ") + "] — call mail_ack to mark as read)", nil
}

func (s *Server) mailAck(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	acked, err := s.Mail.Ack(ctx, c.agent, raw(args, "ids"))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Acked %d message(s) for %s: [%s]", acked.Acked, acked.Agent, strings.Join(acked.AckedIDs, ", ")), nil
}

func (s *Server) mailEdit(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	id, _ := str(args, "id")
	subject, _ := str(args, "subject")
	body, _ := str(args, "body")
	edited, err := s.Mail.Edit(ctx, id, c.agent, sanitize.Content(subject), sanitize.Content(body))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Mail %s updated (to: %s, subject: \"%s\")", edited.ID, edited.ToAgent, edited.Subject), nil
}

func (s *Server) mailUnsend(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	id, _ := str(args, "id")
	unsent, err := s.Mail.Unsend(ctx, id, c.agent)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Mail %s unsent (was to: %s, subject: \"%s\")", unsent.ID, unsent.ToAgent, unsent.Subject), nil
}

// listOptions are mail_sent's and mail_history's filters: the peer (`to` or
// `from`) through sanitize.agentName, and limit and offset as given.
func listOptions(args *jsutil.Object, peerKey string) mail.ListOptions {
	peer, _ := str(args, peerKey)
	return mail.ListOptions{Peer: sanitize.AgentName(peer), Limit: raw(args, "limit"), Offset: raw(args, "offset")}
}

func (s *Server) mailSent(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	agentName, err := identity(args, "agent", c)
	if err != nil {
		return "", err
	}
	sent, err := s.Mail.Sent(ctx, agentName, listOptions(args, "to"))
	if err != nil {
		return "", err
	}
	if len(sent.Messages) == 0 {
		return "No sent mail found.", nil
	}
	blocks := make([]string, len(sent.Messages))
	for i, m := range sent.Messages {
		status := "unread"
		if m.AckedAt != nil {
			status = "read"
		}
		line := fmt.Sprintf("**To:** %s [%s] | **Date:** %s | **Subject:** %s | **ID:** %s", m.ToAgent, status, s.dateText(m.SentAt), m.Subject, m.ID)
		if m.InReplyTo != nil && *m.InReplyTo != "" {
			line += " | **In-Reply-To:** " + *m.InReplyTo
		}
		blocks[i] = line + preview(m.BodyPreview)
	}
	return strings.Join(blocks, "\n\n"), nil
}

func (s *Server) mailHistory(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	agentName, err := identity(args, "agent", c)
	if err != nil {
		return "", err
	}
	history, err := s.Mail.History(ctx, agentName, listOptions(args, "from"))
	if err != nil {
		return "", err
	}
	if len(history.Messages) == 0 {
		return "No mail history found.", nil
	}
	blocks := make([]string, len(history.Messages))
	for i, m := range history.Messages {
		blocks[i] = s.mailBlock(m.FromAgent, m.SentAt, m.Subject, m.ID, m.InReplyTo, m.Body)
	}
	return strings.Join(blocks, "\n\n---\n\n"), nil
}

// rosterSQL is agent_status's roster: every agent with the caller's unread
// chat (outside discussions) and mail from it, online agents first.
const rosterSQL = `SELECT
                a.agent,
                a.status,
                a.last_seen,
                a.expertise,
                a.provider,
                a.model,
                a.virtual,
                a.active_since,
                COALESCE(c.unread_count, 0)::int AS unread_chat,
                COALESCE(m.unread_count, 0)::int AS unread_mail
            FROM agent_status a
            LEFT JOIN (
                SELECT cmt.from_actor_id, COUNT(*) AS unread_count
                FROM chat_messages cm
                JOIN chat_message_texts cmt ON cmt.id = cm.message_text_id
                WHERE cm.to_actor_id = $1 AND cm.acked_at IS NULL AND cm.deleted_at IS NULL AND cmt.discussion_id IS NULL
                GROUP BY cmt.from_actor_id
            ) c ON c.from_actor_id = a.actor_id
            LEFT JOIN (
                SELECT ml.from_actor_id, COUNT(*) AS unread_count
                FROM mail ml
                WHERE ml.to_actor_id = $1 AND ml.acked_at IS NULL AND ml.deleted_at IS NULL
                GROUP BY ml.from_actor_id
            ) m ON m.from_actor_id = a.actor_id`

const rosterOrder = ` ORDER BY CASE a.status WHEN 'online' THEN 0 WHEN 'available' THEN 1 WHEN 'degraded' THEN 2 WHEN 'offline' THEN 3 ELSE 4 END, a.last_seen DESC NULLS LAST`

// errNoJoin is V8's TypeError for `expertise.join(...)` on a value with a
// length but no join method.
var errNoJoin = errors.New("expertise.join is not a function")

// expertiseText is the roster line's expertise: `a.expertise || []`, joined
// with ", " when its length is above 0. ok is false when the line has no
// expertise part. raw is the jsonb text (nil for NULL).
func expertiseText(raw []byte) (text string, ok bool, err error) {
	if raw == nil {
		return "", false, nil
	}
	v, err := jsutil.Parse(raw)
	if err != nil {
		return "", false, err
	}
	if !jsutil.Truthy(v) {
		return "", false, nil
	}
	switch x := v.(type) {
	case []any:
		if len(x) == 0 {
			return "", false, nil
		}
		parts := make([]string, len(x))
		for i, e := range x {
			if e == nil || e == jsutil.Undefined {
				continue
			}
			if parts[i], err = jsutil.ValueString(e); err != nil {
				return "", false, err
			}
		}
		return strings.Join(parts, ", "), true, nil
	case string:
		// A truthy string has a length above 0 and no join method.
		return "", false, errNoJoin
	case *jsutil.Object:
		n, err := jsutil.ValueNumber(x.Value("length"))
		if err != nil {
			return "", false, err
		}
		if n > 0 {
			return "", false, errNoJoin
		}
	}
	// A number or true has no length.
	return "", false, nil
}

func (s *Server) agentStatus(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	if _, err := identity(args, "agent", c); err != nil {
		return "", err
	}
	visible, err := s.Visibility.VisibleActorIDs(ctx, c.actorID)
	if err != nil {
		return "", err
	}
	sql := rosterSQL
	params := []any{c.actorID}
	if !visible.All {
		sql += " WHERE a.actor_id = ANY($2)"
		params = append(params, visible.IDs)
	}
	rows, err := s.DB.Query(ctx, sql+rosterOrder, params...)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var name string
		var status, provider, model *string
		var lastSeen, activeSince *time.Time
		var expertise []byte
		var virtual *bool
		var unreadChat, unreadMail *int
		if err := rows.Scan(&name, &status, &lastSeen, &expertise, &provider, &model, &virtual, &activeSince, &unreadChat, &unreadMail); err != nil {
			return "", err
		}
		parts := []string{name + ": " + orText(status)}
		if activeSince != nil {
			parts = append(parts, "ACTIVE")
		}
		if virtual != nil && *virtual {
			parts = append(parts, "virtual")
		}
		// [a.provider, a.model].filter(Boolean).join('/')
		var ident []string
		for _, p := range []*string{provider, model} {
			if p != nil && *p != "" {
				ident = append(ident, *p)
			}
		}
		if len(ident) > 0 {
			parts = append(parts, strings.Join(ident, "/"))
		}
		if lastSeen != nil {
			parts = append(parts, "last seen "+jsutil.DateString(*lastSeen, s.location()))
		}
		text, ok, err := expertiseText(expertise)
		if err != nil {
			return "", err
		}
		if ok {
			parts = append(parts, "expertise: "+text)
		}
		parts = append(parts, fmt.Sprintf("%d unread chat", orZero(unreadChat)), fmt.Sprintf("%d unread mail", orZero(unreadMail)))
		lines = append(lines, strings.Join(parts, " | "))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "No agents registered.", nil
	}
	return strings.Join(lines, "\n"), nil
}

// orZero is `n || 0` for a nullable integer column.
func orZero(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

func (s *Server) updateExpertise(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	// validateToolArgs has made expertise an array of strings.
	cleaned := []string{}
	for _, e := range arrayOf(args, "expertise") {
		v, ok := e.(string)
		if !ok {
			continue
		}
		v = jsutil.Trim(v)
		if v == "" {
			continue
		}
		cleaned = append(cleaned, sanitize.Content(jsutil.ToLower(v)))
	}
	stored, err := jsonutil.Marshal(cleaned)
	if err != nil {
		return "", err
	}
	if _, err := s.DB.Exec(ctx, "UPDATE actors SET expertise = $1 WHERE id = $2", string(stored), c.actorID); err != nil {
		return "", err
	}
	list := strings.Join(cleaned, ", ")
	if list == "" {
		list = "(none)"
	}
	return "Expertise updated for " + c.agent + ": " + list, nil
}

func (s *Server) updateProfile(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	provider, hasProvider := str(args, "provider")
	model, hasModel := str(args, "model")
	if !hasProvider && !hasModel {
		return "", apierr.New(http.StatusBadRequest, "At least one field required: provider, model")
	}
	var sets, said []string
	var values []any
	if hasProvider {
		values = append(values, sanitize.Identifier(provider))
		sets = append(sets, fmt.Sprintf("provider = $%d", len(values)))
		said = append(said, "provider: "+provider)
	}
	if hasModel {
		values = append(values, sanitize.Identifier(model))
		sets = append(sets, fmt.Sprintf("model = $%d", len(values)))
		said = append(said, "model: "+model)
	}
	values = append(values, c.actorID)
	sql := fmt.Sprintf("UPDATE agent_configuration SET %s WHERE actor_id = $%d", strings.Join(sets, ", "), len(values))
	if _, err := s.DB.Exec(ctx, sql, values...); err != nil {
		return "", err
	}
	return "Profile updated for " + c.agent + ": " + strings.Join(said, ", "), nil
}

func (s *Server) activityStart(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	if _, err := s.DB.Exec(ctx, "UPDATE actors SET active_since = NOW() WHERE id = $1", c.actorID); err != nil {
		return "", err
	}
	s.Events.Broadcast("agent_activity", activityEvent{Agent: c.agent, Active: true})
	return "Activity started for " + c.agent + ".", nil
}

func (s *Server) activityStop(ctx context.Context, args *jsutil.Object, c caller) (string, error) {
	if _, err := s.DB.Exec(ctx, "UPDATE actors SET active_since = NULL WHERE id = $1", c.actorID); err != nil {
		return "", err
	}
	s.Events.Broadcast("agent_activity", activityEvent{Agent: c.agent, Active: false})
	return "Activity stopped for " + c.agent + ".", nil
}
