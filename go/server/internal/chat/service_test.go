package chat

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/system"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

var sentAt = time.Date(2026, 11, 5, 12, 0, 0, 123456000, time.UTC)

const sentAtJS = "2026-11-05T12:00:00.123Z"

// fakeAgents records direct-chat dispatches and returns a fixed reply.
type fakeAgents struct {
	mu     sync.Mutex
	calls  []DirectChat
	reply  json.RawMessage
	err    error
	called chan struct{}
}

func (f *fakeAgents) UnanswerableNotice(agentName, reason string, missing []string) string {
	return "[Error] no access to " + agentName
}

func (f *fakeAgents) HandleDirectChat(ctx context.Context, in DirectChat) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, in)
	f.mu.Unlock()
	if f.called != nil {
		f.called <- struct{}{}
	}
	return f.reply, f.err
}

type recordedEvent struct {
	name string
	data any
}

type fakeEvents struct {
	mu     sync.Mutex
	events []recordedEvent
}

func (f *fakeEvents) Broadcast(event string, data any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, recordedEvent{event, data})
}

func newService(t *testing.T) (*Service, pgxmock.PgxPoolIface, *fakeAgents, *fakeEvents) {
	mock := testutil.NewMock(t)
	cache := actors.NewCache(mock)
	log := logger.New(mock, cache, "")
	agents := &fakeAgents{}
	ev := &fakeEvents{}
	handler := &system.Handler{Log: log}
	return &Service{
		DB: mock, Actors: cache, Log: log, Events: ev, Agents: agents, System: handler,
		Notifier: &system.Notifier{DB: mock, Actors: cache, Handler: handler},
	}, mock, agents, ev
}

func expectActor(mock pgxmock.PgxPoolIface, name string, id int) {
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs(name).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(id, name))
}

func expectActors(mock pgxmock.PgxPoolIface, names []string, ids []int) {
	rows := pgxmock.NewRows([]string{"id", "name"})
	// Only the first len(ids) names exist.
	for i, id := range ids {
		rows.AddRow(id, names[i])
	}
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name = ANY").WithArgs(names).WillReturnRows(rows)
}

// expectText takes the ten insert parameters; a nil in a *string position
// stands for a typed nil.
func expectText(mock pgxmock.PgxPoolIface, id int, args ...any) {
	for _, i := range []int{0, 3, 4, 5, 6, 7, 9} {
		if args[i] == nil {
			args[i] = (*string)(nil)
		}
	}
	mock.ExpectQuery("INSERT INTO chat_message_texts").WithArgs(args...).
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(id, sentAt))
}

func expectDelivery(mock pgxmock.PgxPoolIface, textID, toActorID int, ack bool, id int) {
	mock.ExpectQuery("INSERT INTO chat_messages").WithArgs(textID, toActorID, ack).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(id))
}

func expectVirtualSender(mock pgxmock.PgxPoolIface, actorID int, virtual bool) {
	mock.ExpectQuery("SELECT agc.virtual FROM agent_configuration agc WHERE agc.actor_id").WithArgs(actorID).
		WillReturnRows(pgxmock.NewRows([]string{"virtual"}).AddRow(virtual))
}

func expectVirtualRecipients(mock pgxmock.PgxPoolIface, ids []int, names ...string) {
	rows := pgxmock.NewRows([]string{"agent"})
	for _, n := range names {
		rows.AddRow(n)
	}
	mock.ExpectQuery("WHERE agc.actor_id = ANY").WithArgs(ids).WillReturnRows(rows)
}

func expectAccess(mock pgxmock.PgxPoolIface, actorID, vaID int, ok bool) {
	mock.ExpectQuery("SELECT EXISTS").WithArgs(actorID, vaID).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(ok))
}

func status(err error) int {
	var e *apierr.Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func str(s string) *string { return &s }

func marshal(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestResolveDiscussionID(t *testing.T) {
	cases := []struct {
		id, channel string
		want        int64
		err         bool
	}{
		{`5`, ``, 5, false},
		{`"7"`, ``, 7, false},
		{`" 8 "`, ``, 8, false},
		{`"0x10"`, ``, 16, false},
		{`"1e2"`, ``, 100, false},
		{`12.0`, ``, 12, false},
		{`4.5`, ``, 0, false},
		{`"4abc"`, ``, 0, false},
		{`"  "`, `"discussion-3"`, 0, false},
		{`0`, ``, 0, false},
		{`-3`, ``, 0, false},
		{`9007199254740992`, ``, 0, false},
		{`9007199254740991`, ``, 9007199254740991, false},
		{`false`, `"discussion-3"`, 0, false},
		{`{}`, ``, 0, false},
		{``, `"discussion-3"`, 3, false},
		{`null`, `"discussion-3"`, 3, false},
		{`""`, `"discussion-3"`, 3, false},
		{``, `"discussion-0"`, 0, false},
		{``, `"discussion-3\n"`, 0, false},
		{``, `"discussion-x"`, 0, false},
		{``, `"discussion-99999999999999999999"`, 0, false},
		{``, `""`, 0, false},
		{``, `0`, 0, false},
		{``, `5`, 0, true},
		{``, `["discussion-3"]`, 0, true},
		{`5`, `["x"]`, 5, false},
	}
	for _, c := range cases {
		var id, ch json.RawMessage
		if c.id != "" {
			id = json.RawMessage(c.id)
		}
		if c.channel != "" {
			ch = json.RawMessage(c.channel)
		}
		got, err := ResolveDiscussionID(id, ch)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("(%s, %s) = %d, %v; want %d, err %v", c.id, c.channel, got, err, c.want, c.err)
		}
	}
}

func TestSendRequiredFields(t *testing.T) {
	s, mock, _, _ := newService(t)
	msg := str("hi")
	cases := []struct {
		in   SendInput
		want string
	}{
		{SendInput{Message: msg, ToAgents: To("b")}, "Required field: from_agent"},
		{SendInput{FromAgent: "a", ToAgents: To("b")}, "Required field: message"},
		{SendInput{FromAgent: "a", Message: msg}, "Required: to_agents (array) or discussion_id"},
	}
	for _, c := range cases {
		_, _, err := s.Send(context.Background(), c.in)
		if status(err) != 400 || err.Error() != c.want {
			t.Errorf("%+v: %v", c.in, err)
		}
	}
	// A tool-results message needs no text.
	expectActor(mock, "a", 11)
	_, _, err := s.Send(context.Background(), SendInput{FromAgent: "a", ToolCallResults: []ToolCallResult{{"c1", "x"}}, ToAgents: ToAgents{Given: true}})
	if status(err) != 400 || err.Error() != "to_agents must be a non-empty array" {
		t.Errorf("not an array: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSendRecipientErrors(t *testing.T) {
	s, mock, _, _ := newService(t)
	expectActor(mock, "a", 11)
	msg := str("hi")
	if _, _, err := s.Send(context.Background(), SendInput{FromAgent: "a", Message: msg, ToAgents: To()}); status(err) != 400 || err.Error() != "to_agents must be a non-empty array" {
		t.Errorf("empty: %v", err)
	}
	if _, _, err := s.Send(context.Background(), SendInput{FromAgent: "a", Message: msg, ToAgents: To("a", "a")}); status(err) != 400 || err.Error() != "No recipients resolved" {
		t.Errorf("only self: %v", err)
	}
	expectActors(mock, []string{"b", "ghost"}, []int{12})
	_, _, err := s.Send(context.Background(), SendInput{FromAgent: "a", Message: msg, ToAgents: To("b", "ghost", "b")})
	if status(err) != 404 || err.Error() != `Agent "ghost" is not registered` {
		t.Errorf("unknown: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A direct message to one agent that is not virtual: one text row, one
// delivery row, the live event and the virtual-agent check.
func TestSendDirect(t *testing.T) {
	s, mock, agents, ev := newService(t)
	expectActor(mock, "agent-a", 11)
	expectActors(mock, []string{"agent-b"}, []int{12})
	mock.ExpectBegin()
	expectText(mock, 100, str("Hello"), 11, nil, nil, (*string)(nil), nil, (*string)(nil), (*string)(nil), false, (*string)(nil))
	expectDelivery(mock, 100, 12, false, 500)
	mock.ExpectCommit()
	expectVirtualSender(mock, 11, false)
	expectVirtualRecipients(mock, []int{12})

	sent, pending, err := s.Send(context.Background(), SendInput{FromAgent: "agent-a", ToAgents: To("agent-b"), Message: str("Hello")})
	if err != nil {
		t.Fatal(err)
	}
	if pending != nil {
		t.Error("pending set without a virtual recipient")
	}
	want := `{"from_agent":"agent-a","to_agents":[{"id":500,"agent":"agent-b","sent_at":"` + sentAtJS + `"}],"sent_at":"` + sentAtJS + `"}`
	if got := marshal(t, sent); got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	if len(ev.events) != 1 || ev.events[0].name != "chat_message" {
		t.Fatalf("events %+v", ev.events)
	}
	wantEvent := `{"id":500,"message_text_id":100,"from_agent":"agent-a","to_agents":["agent-b"],"message":"Hello","discussion_id":null,"scene_id":null,"conversation_id":null,"sent_at":"` + sentAtJS + `"}`
	if got := marshal(t, ev.events[0].data); got != wantEvent {
		t.Errorf("event %s\nwant %s", got, wantEvent)
	}
	if len(agents.calls) != 0 {
		t.Errorf("dispatched %+v", agents.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Tool results: one text row per result, tool_calls and tools_offered on
// the first only; one delivery row per text row and recipient. The answer
// names each recipient's last row; each event names the first recipient's
// row for its text.
func TestSendToolCallResults(t *testing.T) {
	s, mock, _, ev := newService(t)
	expectActor(mock, "salem-engine", 1)
	expectActors(mock, []string{"npc-1", "npc-2"}, []int{21, 22})
	mock.ExpectBegin()
	calls := `[{"id":"c1","b":2,"a":1}]`
	expectText(mock, 100, str("r1"), 1, nil, str(`[{"id":"c1","b":2,"a":1}]`), str("c1"), str(`["speak"]`), str("6d2a0c1e-0000-4000-8000-000000000001"), (*string)(nil), false, str("sc-1"))
	expectText(mock, 101, str("r2"), 1, nil, nil, str("c2"), nil, str("6d2a0c1e-0000-4000-8000-000000000001"), (*string)(nil), false, str("sc-1"))
	expectDelivery(mock, 100, 21, false, 500)
	expectDelivery(mock, 101, 21, false, 501)
	expectDelivery(mock, 100, 22, false, 502)
	expectDelivery(mock, 101, 22, false, 503)
	mock.ExpectCommit()

	sent, _, err := s.Send(context.Background(), SendInput{
		FromAgent: "salem-engine", ToAgents: To("npc-1", "npc-2"),
		ToolCalls: json.RawMessage(calls), ToolsOffered: json.RawMessage(`["speak"]`),
		ToolCallResults: []ToolCallResult{{"c1", "r1"}, {"c2", "r2"}}, PersistOnly: true,
		SceneID: str("6d2a0c1e-0000-4000-8000-000000000001"), ConversationID: str("sc-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"from_agent":"salem-engine","to_agents":[{"id":501,"agent":"npc-1","sent_at":"` + sentAtJS + `"},{"id":503,"agent":"npc-2","sent_at":"` + sentAtJS + `"}],"sent_at":"` + sentAtJS + `"}`
	if got := marshal(t, sent); got != want {
		t.Errorf("got %s\nwant %s", got, want)
	}
	if len(ev.events) != 2 {
		t.Fatalf("events %+v", ev.events)
	}
	for i, wantID := range []int{500, 501} {
		if e := ev.events[i].data.(chatEvent); e.ID != wantID || e.MessageTextID != 100+i {
			t.Errorf("event %d: %+v", i, e)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSendEveryone(t *testing.T) {
	s, mock, _, _ := newService(t)
	expectActor(mock, "agent-a", 11)
	mock.ExpectQuery("WHERE ac.name != ").WithArgs("agent-a").
		WillReturnRows(pgxmock.NewRows([]string{"agent"}).AddRow("system").AddRow("agent-b"))
	expectActors(mock, []string{"system", "agent-b"}, []int{1, 12})
	mock.ExpectBegin()
	expectText(mock, 100, str("all"), 11, nil, nil, (*string)(nil), nil, (*string)(nil), (*string)(nil), false, (*string)(nil))
	expectDelivery(mock, 100, 1, false, 500)
	expectDelivery(mock, 100, 12, false, 501)
	mock.ExpectCommit()
	expectVirtualSender(mock, 11, false)
	expectVirtualRecipients(mock, []int{1, 12})

	sent, _, err := s.Send(context.Background(), SendInput{FromAgent: "agent-a", ToAgents: ToAgents{Given: true, Names: []string{"*"}}, Message: str("all")})
	if err != nil {
		t.Fatal(err)
	}
	if len(sent.ToAgents) != 2 || sent.ToAgents[0].Agent != "system" || sent.ToAgents[1].ID != 501 {
		t.Errorf("%+v", sent.ToAgents)
	}
	testutil.WaitMet(t, mock)
}

// A discussion message goes to the joined participants and triggers the
// discussion's virtual agents through a system-to-system message, written
// in the background; no direct-chat check runs.
// expectDiscussionPost expects agent-a's post to discussion 7 (one other
// participant), up to the commit.
func expectDiscussionPost(mock pgxmock.PgxPoolIface) {
	expectActor(mock, "agent-a", 11)
	mock.ExpectQuery("FROM discussion_participants dp").WithArgs(jsutil.Num(7), "joined", 11).
		WillReturnRows(pgxmock.NewRows([]string{"agent"}).AddRow("agent-b"))
	expectActors(mock, []string{"agent-b"}, []int{12})
	mock.ExpectBegin()
	expectText(mock, 100, str("in the room"), 11, jsutil.Num(7), nil, (*string)(nil), nil, (*string)(nil), (*string)(nil), false, (*string)(nil))
	expectDelivery(mock, 100, 12, false, 500)
	mock.ExpectCommit()
	expectActor(mock, "system", 1)
}

const discussionNotice = `{"type":"virtual-agent","discussionId":7,"triggerType":"message"}`

func expectNoticeText(mock pgxmock.PgxPoolIface) *pgxmock.ExpectedQuery {
	return mock.ExpectQuery(`INSERT INTO chat_message_texts \(message, from_actor_id, discussion_id, sent_at\)`).
		WithArgs(discussionNotice, 1, nil)
}

func expectNoticeDelivery(mock pgxmock.PgxPoolIface) {
	mock.ExpectQuery(`INSERT INTO chat_messages \(message_text_id, to_actor_id\)`).WithArgs(101, 1).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(501))
}

func sendDiscussionPost(ctx context.Context, s *Service) error {
	_, _, err := s.Send(ctx, SendInput{FromAgent: "agent-a", DiscussionID: 7, Message: str("in the room")})
	return err
}

// A discussion post notifies the system with a virtual-agent "message"
// payload. Send waits for the notification's two rows, so they take their
// ids before the caller's next write: with the first insert held for
// 150 ms, Send takes at least that long and both inserts are done when it
// returns.
func TestSendDiscussion(t *testing.T) {
	s, mock, _, _ := newService(t)
	expectDiscussionPost(mock)
	expectNoticeText(mock).WillDelayFor(150 * time.Millisecond).
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(101, sentAt))
	expectNoticeDelivery(mock)

	start := time.Now()
	if err := sendDiscussionPost(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("Send returned after %v, before the notification was written", elapsed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A failed notification does not fail the committed send, as Node's
// .catch(() => {}) did.
func TestSendDiscussionNoticeFails(t *testing.T) {
	s, mock, _, _ := newService(t)
	expectDiscussionPost(mock)
	expectNoticeText(mock).WillReturnError(errors.New("connection reset"))

	if err := sendDiscussionPost(context.Background(), s); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A client that hangs up while the notification is being written does not
// stop it: the send is committed, as in Node.
func TestSendDiscussionNoticeSurvivesHangUp(t *testing.T) {
	s, mock, _, _ := newService(t)
	expectDiscussionPost(mock)
	expectNoticeText(mock).WillDelayFor(300 * time.Millisecond).
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(101, sentAt))
	expectNoticeDelivery(mock)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(100*time.Millisecond, cancel)
	if err := sendDiscussionPost(ctx, s); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("the request was not cancelled during the notification")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A sender without access to a virtual agent gets the notice back from it,
// for a non-wait message to that one agent only.
func TestSendVirtualAgentWithoutAccess(t *testing.T) {
	s, mock, agents, _ := newService(t)
	expectActor(mock, "agent-a", 11)
	expectActors(mock, []string{"helper-va"}, []int{30})
	mock.ExpectBegin()
	expectText(mock, 100, str("help?"), 11, nil, nil, (*string)(nil), nil, (*string)(nil), (*string)(nil), false, (*string)(nil))
	expectDelivery(mock, 100, 30, false, 500)
	mock.ExpectCommit()
	expectVirtualSender(mock, 11, false)
	expectVirtualRecipients(mock, []int{30}, "helper-va")
	expectAccess(mock, 11, 30, false)
	// The notice, sent as the virtual agent (both actors are cached now); its
	// own check stops at the virtual sender.
	mock.ExpectBegin()
	expectText(mock, 101, str("[Error] no access to helper-va"), 30, nil, nil, (*string)(nil), nil, (*string)(nil), (*string)(nil), true, (*string)(nil))
	expectDelivery(mock, 101, 11, false, 501)
	mock.ExpectCommit()
	expectVirtualSender(mock, 30, true)

	_, pending, err := s.Send(context.Background(), SendInput{FromAgent: "agent-a", ToAgents: To("helper-va"), Message: str("help?")})
	if err != nil {
		t.Fatal(err)
	}
	if pending != nil || len(agents.calls) != 0 {
		t.Errorf("pending %v calls %+v", pending != nil, agents.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// wait=true with one virtual recipient the sender may use: the reply is
// pending for the route, with the reply row acked on insert.
func TestSendWaitPending(t *testing.T) {
	s, mock, agents, _ := newService(t)
	agents.reply = json.RawMessage(`{"text":"hi"}`)
	expectActor(mock, "salem-engine", 1)
	expectActors(mock, []string{"npc"}, []int{21})
	mock.ExpectBegin()
	expectText(mock, 100, str("tick"), 1, nil, nil, str("c9"), str(`[]`), (*string)(nil), (*string)(nil), false, (*string)(nil))
	expectDelivery(mock, 100, 21, false, 500)
	mock.ExpectCommit()
	expectVirtualSender(mock, 1, false)
	expectVirtualRecipients(mock, []int{21}, "npc")
	expectAccess(mock, 1, 21, true)

	_, pending, err := s.Send(context.Background(), SendInput{
		FromAgent: "salem-engine", ToAgents: To("npc"), Message: str("tick"), ToolCallID: str("c9"),
		ToolsOffered: json.RawMessage(`[]`), HistoryScope: str("scene"), Wait: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pending == nil {
		t.Fatal("no pending reply")
	}
	reply, err := pending(context.Background())
	if err != nil || string(reply) != `{"text":"hi"}` {
		t.Errorf("reply %s %v", reply, err)
	}
	if len(agents.calls) != 1 {
		t.Fatalf("calls %+v", agents.calls)
	}
	c := agents.calls[0]
	if c.VirtualAgent != "npc" || c.MessageID != 500 || !c.IsToolResultCall || !c.AckReplyOnInsert || c.NotifyUnanswerable ||
		c.HistoryScope == nil || *c.HistoryScope != "scene" || string(c.ToolsOffered) != `[]` {
		t.Errorf("call %+v", c)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Without wait, a single dispatch runs in the background and notifies on
// failure; a failed eligibility check never fails the send.
func TestSendDispatchInBackground(t *testing.T) {
	s, mock, agents, _ := newService(t)
	agents.called = make(chan struct{}, 1)
	expectActor(mock, "agent-a", 11)
	expectActors(mock, []string{"helper-va"}, []int{30})
	mock.ExpectBegin()
	expectText(mock, 100, str("hi"), 11, nil, nil, (*string)(nil), nil, (*string)(nil), (*string)(nil), false, (*string)(nil))
	expectDelivery(mock, 100, 30, false, 500)
	mock.ExpectCommit()
	expectVirtualSender(mock, 11, false)
	expectVirtualRecipients(mock, []int{30}, "helper-va")
	expectAccess(mock, 11, 30, true)

	_, pending, err := s.Send(context.Background(), SendInput{FromAgent: "agent-a", ToAgents: To("helper-va"), Message: str("hi")})
	if err != nil || pending != nil {
		t.Fatalf("%v %v", err, pending != nil)
	}
	select {
	case <-agents.called:
	case <-time.After(2 * time.Second):
		t.Fatal("no dispatch")
	}
	if c := agents.calls[0]; !c.NotifyUnanswerable || c.AckReplyOnInsert || c.IsToolResultCall {
		t.Errorf("call %+v", c)
	}

	s2, mock2, _, _ := newService(t)
	expectActor(mock2, "agent-a", 11)
	expectActors(mock2, []string{"agent-b"}, []int{12})
	mock2.ExpectBegin()
	expectText(mock2, 100, str("hi"), 11, nil, nil, (*string)(nil), nil, (*string)(nil), (*string)(nil), false, (*string)(nil))
	expectDelivery(mock2, 100, 12, false, 500)
	mock2.ExpectCommit()
	mock2.ExpectQuery("SELECT agc.virtual").WithArgs(11).WillReturnError(errors.New("boom"))
	if _, _, err := s2.Send(context.Background(), SendInput{FromAgent: "agent-a", ToAgents: To("agent-b"), Message: str("hi")}); err != nil {
		t.Errorf("eligibility error failed the send: %v", err)
	}
}

// cancelOnEvent cancels the request when the first live event goes out,
// which is right after the commit.
type cancelOnEvent struct{ cancel context.CancelFunc }

func (c cancelOnEvent) Broadcast(string, any) { c.cancel() }

// ctxDB fails a call whose context is done, as the real pool does.
type ctxDB struct{ db.DB }

func (d ctxDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return d.DB.Query(ctx, sql, args...)
}

func (d ctxDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := ctx.Err(); err != nil {
		return errRow{err}
	}
	return d.DB.QueryRow(ctx, sql, args...)
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// A client that hangs up after the commit still gets its message to the
// virtual agent: the check after the commit does not use the request's
// context.
func TestSendDispatchSurvivesHangUp(t *testing.T) {
	s, mock, agents, _ := newService(t)
	s.DB = ctxDB{mock}
	agents.called = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Events = cancelOnEvent{cancel}
	expectActor(mock, "agent-a", 11)
	expectActors(mock, []string{"helper-va"}, []int{30})
	mock.ExpectBegin()
	expectText(mock, 100, str("hi"), 11, nil, nil, (*string)(nil), nil, (*string)(nil), (*string)(nil), false, (*string)(nil))
	expectDelivery(mock, 100, 30, false, 500)
	mock.ExpectCommit()
	expectVirtualSender(mock, 11, false)
	expectVirtualRecipients(mock, []int{30}, "helper-va")
	expectAccess(mock, 11, 30, true)

	if _, _, err := s.Send(ctx, SendInput{FromAgent: "agent-a", ToAgents: To("helper-va"), Message: str("hi")}); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("the request was not cancelled")
	}
	select {
	case <-agents.called:
	case <-time.After(2 * time.Second):
		t.Fatal("no dispatch after the hang-up")
	}
	testutil.WaitMet(t, mock)
}

// A failed insert rolls the transaction back and fails the send.
func TestSendRollsBack(t *testing.T) {
	s, mock, _, ev := newService(t)
	expectActor(mock, "agent-a", 11)
	expectActors(mock, []string{"agent-b"}, []int{12})
	mock.ExpectBegin()
	expectText(mock, 100, str("x"), 11, nil, nil, (*string)(nil), nil, (*string)(nil), (*string)(nil), false, (*string)(nil))
	mock.ExpectQuery("INSERT INTO chat_messages").WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).WillReturnError(errors.New("insert failed"))
	mock.ExpectRollback()
	if _, _, err := s.Send(context.Background(), SendInput{FromAgent: "agent-a", ToAgents: To("agent-b"), Message: str("x")}); err == nil || err.Error() != "insert failed" {
		t.Errorf("err %v", err)
	}
	if len(ev.events) != 0 {
		t.Errorf("events after a rollback: %+v", ev.events)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestSendLoneSurrogateInToolCalls(t *testing.T) {
	s, mock, _, _ := newService(t)
	expectActor(mock, "agent-a", 11)
	expectActors(mock, []string{"agent-b"}, []int{12})
	_, _, err := s.Send(context.Background(), SendInput{FromAgent: "agent-a", ToAgents: To("agent-b"), Message: str("x"), ToolCalls: json.RawMessage(`["\udc00"]`)})
	if err == nil || status(err) != 0 {
		t.Errorf("err %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestReceive(t *testing.T) {
	s, mock, _, _ := newService(t)
	expectActor(mock, "agent-a", 11)
	expectActor(mock, "agent-b", 12)
	after := 40.0
	mock.ExpectQuery(`WHERE cm.to_actor_id = \$1 AND cm.acked_at IS NULL AND cm.deleted_at IS NULL AND cmt.discussion_id = \$2 AND cmt.from_actor_id = \$3 AND cm.id > \$4 ORDER BY cm.id ASC`).
		WithArgs(11, jsutil.Num(7), 12, jsutil.Num(40)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "from_agent", "to_agent", "message", "sent_at", "tool_calls", "tool_call_id", "tools_offered"}).
			AddRow(41, "agent-b", "agent-a", "hi", sentAt, []byte(`[{"b": 1, "a": 2}]`), str("c1"), []byte(nil)))
	got, err := s.Receive(context.Background(), "agent-a", 7, &after, "agent-b")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"messages":[{"id":41,"from_agent":"agent-b","to_agent":"agent-a","message":"hi","sent_at":"` + sentAtJS + `","tool_calls":[{"b":1,"a":2}],"tool_call_id":"c1","tools_offered":null}],"pending_count":1}`
	if g := marshal(t, got); g != want {
		t.Errorf("got %s\nwant %s", g, want)
	}

	expectActor(mock, "agent-c", 13)
	mock.ExpectQuery(`AND cmt.discussion_id IS NULL ORDER BY cm.id ASC`).WithArgs(13).
		WillReturnRows(pgxmock.NewRows([]string{"id", "from_agent", "to_agent", "message", "sent_at", "tool_calls", "tool_call_id", "tools_offered"}))
	got, err = s.Receive(context.Background(), "agent-c", 0, nil, "")
	if err != nil || marshal(t, got) != `{"messages":[],"pending_count":0}` {
		t.Errorf("%v %v", got, err)
	}
	if _, err := s.Receive(context.Background(), "", 0, nil, ""); status(err) != 400 || err.Error() != "Required field: agent" {
		t.Errorf("no agent: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestAck(t *testing.T) {
	s, mock, _, _ := newService(t)
	for _, ids := range []string{``, `[]`, `"5"`, `{}`} {
		var raw json.RawMessage
		if ids != "" {
			raw = json.RawMessage(ids)
		}
		if _, err := s.Ack(context.Background(), "agent-a", raw); status(err) != 400 || err.Error() != "Required fields: agent, message_ids (non-empty array)" {
			t.Errorf("%s: %v", ids, err)
		}
	}
	expectActor(mock, "agent-a", 11)
	mock.ExpectQuery("UPDATE chat_messages SET acked_at = NOW").WithArgs(`{"5","x",NULL}`, 11).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(5))
	got, err := s.Ack(context.Background(), "agent-a", json.RawMessage(`[5,"x",null]`))
	if err != nil || marshal(t, got) != `{"agent":"agent-a","acked":1,"acked_ids":[5]}` {
		t.Errorf("%v %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestStatus(t *testing.T) {
	s, mock, _, _ := newService(t)
	expectActor(mock, "agent-a", 11)
	mock.ExpectQuery(`SELECT COUNT\(\*\) as count .* AND cm.acked_at IS NULL AND cm.deleted_at IS NULL AND cmt.discussion_id = \$2`).WithArgs(11, jsutil.Num(7)).
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int64(3)))
	mock.ExpectQuery(`SELECT MAX\(cm.id\) as max_id`).WithArgs(11, jsutil.Num(7)).
		WillReturnRows(pgxmock.NewRows([]string{"max_id"}).AddRow(42))
	mock.ExpectQuery(`SELECT MAX\(cmt.sent_at\) as last_sent`).WithArgs(11, jsutil.Num(7)).
		WillReturnRows(pgxmock.NewRows([]string{"last_sent"}).AddRow(sentAt))
	mock.ExpectQuery(`SELECT MAX\(cm.acked_at\) as last_acked`).WithArgs(11, jsutil.Num(7)).
		WillReturnRows(pgxmock.NewRows([]string{"last_acked"}).AddRow(nil))
	got, err := s.Status(context.Background(), "agent-a", 7)
	want := `{"agent":"agent-a","pending_count":3,"max_message_id":42,"last_message_at":"` + sentAtJS + `","last_ack_at":null}`
	if err != nil || marshal(t, got) != want {
		t.Errorf("%s %v", marshal(t, got), err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
