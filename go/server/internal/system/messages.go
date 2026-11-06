package system

import (
	"context"
	"time"
)

// Sent is one delivery SendSystemMessageToMany wrote.
type Sent struct {
	Agent  string
	ID     int
	SentAt time.Time
}

// SendSystemMessage stores message as a chat message from system to
// toAgent: one text row and one delivery, two separate statements as in
// Node's sendSystemMessage. discussionID is the text row's discussion_id
// parameter (nil for none).
func (n *Notifier) SendSystemMessage(ctx context.Context, toAgent, message string, discussionID any) (*Notified, error) {
	systemActor, err := n.Actors.RequireByName(ctx, "system")
	if err != nil {
		return nil, err
	}
	toActor, err := n.Actors.RequireByName(ctx, toAgent)
	if err != nil {
		return nil, err
	}
	return n.insert(ctx, message, systemActor.ID, discussionID, toActor.ID)
}

// SendSystemMessageToMany sends message to each agent in order and stops at
// the first failure, as Node's loop did.
func (n *Notifier) SendSystemMessageToMany(ctx context.Context, toAgents []string, message string, discussionID any) ([]Sent, error) {
	out := make([]Sent, 0, len(toAgents))
	for _, agent := range toAgents {
		row, err := n.SendSystemMessage(ctx, agent, message, discussionID)
		if err != nil {
			return out, err
		}
		out = append(out, Sent{Agent: agent, ID: row.ID, SentAt: row.SentAt})
	}
	return out, nil
}

// SendDiscussionEvent posts an event line to a discussion: a message from
// system to system, which the admin UI shows and the discuss transports
// skip. discussionID is the discussion_id parameter (nil for none).
func (n *Notifier) SendDiscussionEvent(ctx context.Context, discussionID any, message string) (*Notified, error) {
	systemActor, err := n.Actors.RequireByName(ctx, "system")
	if err != nil {
		return nil, err
	}
	return n.insert(ctx, message, systemActor.ID, discussionID, systemActor.ID)
}

// NotifyDiscussionInvite tells each invited agent about a new discussion.
// discussionID is the id as Node's template string printed it.
func (n *Notifier) NotifyDiscussionInvite(ctx context.Context, discussionID, topic, createdBy string, invited []string) ([]Sent, error) {
	message := "You've been invited to discussion #" + discussionID + `: "` + topic + `" (created by ` + createdBy + ")"
	return n.SendSystemMessageToMany(ctx, invited, message, nil)
}

func (n *Notifier) insert(ctx context.Context, message string, fromID int, discussionID any, toID int) (*Notified, error) {
	var textID int
	var sentAt time.Time
	if err := n.DB.QueryRow(ctx,
		"INSERT INTO chat_message_texts (message, from_actor_id, discussion_id, sent_at) VALUES ($1, $2, $3, NOW()) RETURNING id, sent_at",
		message, fromID, discussionID).Scan(&textID, &sentAt); err != nil {
		return nil, err
	}
	var id int
	if err := n.DB.QueryRow(ctx,
		"INSERT INTO chat_messages (message_text_id, to_actor_id) VALUES ($1, $2) RETURNING id",
		textID, toID).Scan(&id); err != nil {
		return nil, err
	}
	return &Notified{ID: id, SentAt: sentAt}, nil
}
