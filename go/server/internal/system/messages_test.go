package system

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

const (
	actorByName  = "SELECT id, name FROM actors WHERE name = $1"
	insertText   = "INSERT INTO chat_message_texts (message, from_actor_id, discussion_id, sent_at) VALUES ($1, $2, $3, NOW()) RETURNING id, sent_at"
	insertSingle = "INSERT INTO chat_messages (message_text_id, to_actor_id) VALUES ($1, $2) RETURNING id"
)

func expectActor(mock pgxmock.PgxPoolIface, name string, id int) {
	mock.ExpectQuery(regexp.QuoteMeta(actorByName)).WithArgs(name).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(id, name))
}

func expectDelivery(mock pgxmock.PgxPoolIface, message string, from int, discussionID any, to, textID, id int, sentAt time.Time) {
	mock.ExpectQuery(regexp.QuoteMeta(insertText)).WithArgs(message, from, discussionID).
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(textID, sentAt))
	mock.ExpectQuery(regexp.QuoteMeta(insertSingle)).WithArgs(textID, to).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(id))
}

func TestSendSystemMessages(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.MatchExpectationsInOrder(true)
	n := &Notifier{DB: mock, Actors: actors.NewCache(mock)}
	sentAt := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	ctx := context.Background()

	// The system actor and each recipient are looked up once (cached after),
	// then each delivery is a text row and a delivery row.
	expectActor(mock, "system", 1)
	expectActor(mock, "agent-a", 2)
	expectDelivery(mock, "hello", 1, nil, 2, 10, 20, sentAt)
	expectActor(mock, "agent-b", 3)
	expectDelivery(mock, "hello", 1, nil, 3, 11, 21, sentAt)
	sent, err := n.SendSystemMessageToMany(ctx, []string{"agent-a", "agent-b"}, "hello", nil)
	if err != nil || len(sent) != 2 || sent[0] != (Sent{"agent-a", 20, sentAt}) || sent[1] != (Sent{"agent-b", 21, sentAt}) {
		t.Fatalf("sent %+v, %v", sent, err)
	}

	// The discussion event goes from system to system, in the discussion.
	expectDelivery(mock, "agent-a joined the discussion", 1, "101", 1, 12, 22, sentAt)
	row, err := n.SendDiscussionEvent(ctx, "101", "agent-a joined the discussion")
	if err != nil || row.ID != 22 {
		t.Fatalf("event %+v, %v", row, err)
	}

	// The invite text, and a stop at the first recipient that fails.
	invite := `You've been invited to discussion #7: "Plan" (created by agent-a)`
	expectDelivery(mock, invite, 1, nil, 3, 13, 23, sentAt)
	mock.ExpectQuery(regexp.QuoteMeta(actorByName)).WithArgs("nobody").WillReturnRows(pgxmock.NewRows([]string{"id", "name"}))
	sent, err = n.NotifyDiscussionInvite(ctx, "7", "Plan", "agent-a", []string{"agent-b", "nobody", "agent-a"})
	if err == nil || err.Error() != `Agent "nobody" is not registered` || len(sent) != 1 {
		t.Fatalf("invite %+v, %v", sent, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSendSystemMessageFailures(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.MatchExpectationsInOrder(true)
	n := &Notifier{DB: mock, Actors: actors.NewCache(mock)}
	ctx := context.Background()
	boom := errors.New("boom")

	mock.ExpectQuery(regexp.QuoteMeta(actorByName)).WithArgs("system").WillReturnError(boom)
	if _, err := n.SendDiscussionEvent(ctx, nil, "x"); !errors.Is(err, boom) {
		t.Errorf("system lookup: %v", err)
	}
	expectActor(mock, "system", 1)
	expectActor(mock, "agent-a", 2)
	mock.ExpectQuery(regexp.QuoteMeta(insertText)).WithArgs("x", 1, nil).WillReturnError(boom)
	if _, err := n.SendSystemMessage(ctx, "agent-a", "x", nil); !errors.Is(err, boom) {
		t.Errorf("text insert: %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(insertText)).WithArgs("x", 1, nil).
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(5, time.Now()))
	mock.ExpectQuery(regexp.QuoteMeta(insertSingle)).WithArgs(5, 2).WillReturnError(boom)
	if _, err := n.SendSystemMessage(ctx, "agent-a", "x", nil); !errors.Is(err, boom) {
		t.Errorf("delivery insert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
