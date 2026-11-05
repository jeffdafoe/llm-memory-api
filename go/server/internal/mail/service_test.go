package mail

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/virtualagent"
)

const mailID = "0a1b2c3d-0000-4000-8000-000000000001"

var sentAt = time.Date(2026, 11, 5, 12, 0, 0, 123456000, time.UTC)

func newService(t *testing.T) (*Service, pgxmock.PgxPoolIface) {
	mock := testutil.NewMock(t)
	cache := actors.NewCache(mock)
	return &Service{DB: mock, Actors: cache, Log: logger.New(mock, cache, ""), Agents: virtualagent.Seam{}}, mock
}

func expectActor(mock pgxmock.PgxPoolIface, name string, id int) {
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs(name).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(id, name))
}

func expectVirtual(mock pgxmock.PgxPoolIface, actorID int, virtual *bool) {
	rows := pgxmock.NewRows([]string{"virtual"})
	if virtual != nil {
		rows.AddRow(virtual)
	}
	mock.ExpectQuery("SELECT virtual FROM agent_configuration").WithArgs(actorID).WillReturnRows(rows)
}

func status(err error) int {
	var e *apierr.Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func TestSendRequiresEveryField(t *testing.T) {
	s, mock := newService(t)
	cases := [][4]string{{"", "a", "s", "b"}, {"t", "", "s", "b"}, {"t", "a", "", "b"}, {"t", "a", "s", ""}}
	for _, c := range cases {
		_, err := s.Send(context.Background(), c[0], c[1], c[2], c[3], "")
		if status(err) != 400 || err.Error() != "Required fields: to_agent, from_agent, subject, body" {
			t.Errorf("%q: %v", c, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Mail from system starts no virtual-agent lookup; the reply column is set
// only for a non-empty in_reply_to.
func TestSendFromSystem(t *testing.T) {
	s, mock := newService(t)
	expectActor(mock, "agent-a", 11)
	expectActor(mock, "system", 1)
	mock.ExpectQuery(`INSERT INTO mail \(to_actor_id, from_actor_id, subject, body\) VALUES`).
		WithArgs(11, 1, "Welcome", "Hello").
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(mailID, sentAt))

	got, err := s.Send(context.Background(), "agent-a", "system", "Welcome", "Hello", "")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(got)
	want := `{"id":"` + mailID + `","to_agent":"agent-a","from_agent":"system","subject":"Welcome","sent_at":"2026-11-05T12:00:00.123Z"}`
	if string(out) != want {
		t.Errorf("got %s\nwant %s", out, want)
	}
	time.Sleep(50 * time.Millisecond)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A sender without access to a virtual recipient gets the notice back, sent
// as the virtual agent, in reply to the original; the notice itself starts
// no further dispatch (its sender is virtual).
func TestSendToVirtualAgentWithoutAccess(t *testing.T) {
	s, mock := newService(t)
	yes, no := true, false
	expectActor(mock, "helper", 20)
	expectActor(mock, "agent-a", 11)
	mock.ExpectQuery(`INSERT INTO mail \(to_actor_id, from_actor_id, subject, body\) VALUES`).
		WithArgs(20, 11, "Question", "Hi").
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(mailID, sentAt))
	expectVirtual(mock, 20, &yes)
	expectVirtual(mock, 11, &no)
	mock.ExpectQuery("SELECT EXISTS").WithArgs(11, 20).
		WillReturnRows(pgxmock.NewRows([]string{"has_access"}).AddRow(false))
	notice := "[Error] You do not have access to helper, so it will not reply. Its owner can grant access in the dashboard (Agents → helper → Permissions)."
	mock.ExpectQuery(`INSERT INTO mail \(to_actor_id, from_actor_id, subject, body, in_reply_to\) VALUES`).
		WithArgs(11, 20, "Re: Question", notice, mailID).
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow("0a1b2c3d-0000-4000-8000-000000000002", sentAt))
	// The notice's own trigger: recipient agent-a is not virtual, so it stops.
	expectVirtual(mock, 11, &no)
	expectVirtual(mock, 20, &yes)

	if _, err := s.Send(context.Background(), "helper", "agent-a", "Question", "Hi", ""); err != nil {
		t.Fatal(err)
	}
	testutil.WaitMet(t, mock)
}

// recordingAgents records the direct-mail dispatch.
type recordingAgents struct {
	virtualagent.Seam
	got chan string
}

func (r recordingAgents) HandleDirectMail(ctx context.Context, va, from, id string) error {
	r.got <- va + "|" + from + "|" + id
	return nil
}

func TestSendToVirtualAgentWithAccessDispatches(t *testing.T) {
	s, mock := newService(t)
	rec := recordingAgents{got: make(chan string, 1)}
	s.Agents = rec
	yes := true
	expectActor(mock, "helper", 20)
	expectActor(mock, "agent-a", 11)
	mock.ExpectQuery(`INSERT INTO mail \(to_actor_id, from_actor_id, subject, body\) VALUES`).
		WithArgs(20, 11, "Re: Question", "Hi").
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(mailID, sentAt))
	expectVirtual(mock, 20, &yes)
	expectVirtual(mock, 11, nil)
	mock.ExpectQuery("SELECT EXISTS").WithArgs(11, 20).
		WillReturnRows(pgxmock.NewRows([]string{"has_access"}).AddRow(true))

	if _, err := s.Send(context.Background(), "helper", "agent-a", "Re: Question", "Hi", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-rec.got:
		if got != "helper|agent-a|"+mailID {
			t.Errorf("dispatch %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no dispatch")
	}
	testutil.WaitMet(t, mock)
}

func TestReceiveValidatesIds(t *testing.T) {
	missing := "Required field: ids (array of mail UUIDs). Use mail_check first to list unread mail with IDs, then call mail_receive with specific IDs to read full content, then call mail_ack with those IDs after processing."
	cases := map[string]string{
		"":                        missing,
		"null":                    missing,
		`"` + mailID + `"`:        missing,
		"[]":                      missing,
		`{"0":"x"}`:               missing,
		`["nope"]`:                "Invalid UUID in ids array",
		`[5]`:                     "Invalid UUID in ids array",
		`["` + mailID + `",null]`: "Invalid UUID in ids array",
	}
	for in, want := range cases {
		s, mock := newService(t)
		expectActor(mock, "agent-a", 11)
		_, err := s.Receive(context.Background(), "agent-a", json.RawMessage(in))
		if status(err) != 400 || err.Error() != want {
			t.Errorf("ids %s: %v", in, err)
		}
	}
}

func TestReceiveSendsTheIdsAsAnArrayLiteral(t *testing.T) {
	s, mock := newService(t)
	expectActor(mock, "agent-b", 12)
	upper := strings.ToUpper(mailID)
	mock.ExpectQuery("WHERE m.id = ANY").WithArgs(`{"`+upper+`"}`, 12).
		WillReturnRows(pgxmock.NewRows([]string{"id", "from_agent", "to_agent", "subject", "body", "sent_at", "in_reply_to"}).
			AddRow(mailID, "agent-a", "agent-b", "S", "B", sentAt, nil))
	got, err := s.Receive(context.Background(), "agent-b", json.RawMessage(` [ "`+upper+`" ] `))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(got)
	want := `{"messages":[{"id":"` + mailID + `","from_agent":"agent-a","to_agent":"agent-b","subject":"S","body":"B","sent_at":"2026-11-05T12:00:00.123Z","in_reply_to":null}]}`
	if string(out) != want {
		t.Errorf("got %s\nwant %s", out, want)
	}
}

// Ack checks only that ids is a non-empty array; its elements reach
// Postgres as node-postgres's array literal.
func TestAck(t *testing.T) {
	s, mock := newService(t)
	for _, in := range []string{"", "null", `"x"`, "[]", "{}"} {
		_, err := s.Ack(context.Background(), "agent-b", json.RawMessage(in))
		if status(err) != 400 || err.Error() != "Required fields: agent, message_ids (array of UUIDs)" {
			t.Errorf("ids %s: %v", in, err)
		}
	}
	if _, err := s.Ack(context.Background(), "", json.RawMessage(`["x"]`)); status(err) != 400 {
		t.Errorf("no agent: %v", err)
	}

	expectActor(mock, "agent-b", 12)
	mock.ExpectQuery("UPDATE mail SET acked_at = NOW()").WithArgs(`{"`+mailID+`","5",NULL}`, 12).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(mailID))
	got, err := s.Ack(context.Background(), "agent-b", json.RawMessage(`["`+mailID+`",5,null]`))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(got)
	if string(out) != `{"agent":"agent-b","acked":1,"acked_ids":["`+mailID+`"]}` {
		t.Errorf("got %s", out)
	}
}

func TestEditBuildsTheSetClause(t *testing.T) {
	s, mock := newService(t)
	if _, err := s.Edit(context.Background(), mailID, "agent-a", "", ""); status(err) != 400 || err.Error() != "At least one of subject or body must be provided" {
		t.Errorf("nothing to change: %v", err)
	}
	if _, err := s.Edit(context.Background(), "", "agent-a", "s", ""); status(err) != 400 || err.Error() != "Required fields: id, from_agent. Optional: subject, body" {
		t.Errorf("no id: %v", err)
	}

	expectActor(mock, "agent-a", 11)
	mock.ExpectQuery(`UPDATE mail SET body = \$1\s+WHERE id = \$2 AND from_actor_id = \$3`).WithArgs("new body", mailID, 11).
		WillReturnRows(pgxmock.NewRows([]string{"id", "subject", "sent_at"}).AddRow(mailID, "S", sentAt))
	mock.ExpectQuery("SELECT ta.name AS to_agent").WithArgs(mailID).
		WillReturnRows(pgxmock.NewRows([]string{"to_agent"}).AddRow("agent-b"))
	got, err := s.Edit(context.Background(), mailID, "agent-a", "", "new body")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(got)
	if string(out) != `{"id":"`+mailID+`","to_agent":"agent-b","subject":"S","sent_at":"2026-11-05T12:00:00.123Z"}` {
		t.Errorf("got %s", out)
	}

	mock.ExpectQuery(`UPDATE mail SET subject = \$1, body = \$2\s+WHERE id = \$3 AND from_actor_id = \$4`).WithArgs("S2", "B2", mailID, 11).
		WillReturnRows(pgxmock.NewRows([]string{"id", "subject", "sent_at"}))
	if _, err := s.Edit(context.Background(), mailID, "agent-a", "S2", "B2"); status(err) != 404 || err.Error() != "Mail not found, not owned by you, or already acked" {
		t.Errorf("acked: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestUnsendNotFound(t *testing.T) {
	s, mock := newService(t)
	expectActor(mock, "agent-a", 11)
	mock.ExpectQuery("UPDATE mail SET deleted_at = NOW()").WithArgs(mailID, 11).
		WillReturnRows(pgxmock.NewRows([]string{"id", "subject"}))
	if _, err := s.Unsend(context.Background(), mailID, "agent-a"); status(err) != 404 {
		t.Errorf("got %v", err)
	}
}

// limit is Math.min(limit || 50, 100) under Number() rules.
func TestLimitParam(t *testing.T) {
	cases := map[string]jsutil.Number{
		"": "50", "null": "50", "0": "50", `""`: "50", "false": "50",
		"20": "20", `"20"`: "20", "500": "100", "-1": "-1", "2.5": "2.5",
		`"abc"`: "NaN", "{}": "NaN", "[]": "0", "[7]": "7", "true": "1", `"0"`: "0", "1e999": "100",
	}
	for in, want := range cases {
		if got, err := limitParam(json.RawMessage(in)); err != nil || got != want {
			t.Errorf("limit %s: %q %v, want %q", in, got, err, want)
		}
	}
	for _, in := range []string{`{"toString":null}`, `[{"toString":1}]`, `[null,[{"toString":"x"}]]`} {
		if _, err := limitParam(json.RawMessage(in)); err != jsutil.ErrNoPrimitive {
			t.Errorf("limit %s: %v, want V8's TypeError", in, err)
		}
	}
	if _, err := limitParam(json.RawMessage(`{"valueOf":null,"a":{"toString":null}}`)); err != nil {
		t.Errorf("valueOf and a nested toString convert to NaN: %v", err)
	}
}

func TestSentFiltersByRecipient(t *testing.T) {
	s, mock := newService(t)
	expectActor(mock, "agent-a", 11)
	expectActor(mock, "agent-b", 12)
	mock.ExpectQuery(`WHERE m.from_actor_id = \$1 AND m.deleted_at IS NULL AND m.to_actor_id = \$2\s+ORDER BY m.sent_at DESC\s+LIMIT \$3 OFFSET \$4`).
		WithArgs(11, 12, jsutil.Number("2"), "1").
		WillReturnRows(pgxmock.NewRows([]string{"id", "to_agent", "subject", "body_preview", "sent_at", "acked_at", "in_reply_to"}).
			AddRow(mailID, "agent-b", "S", "prev", sentAt, &sentAt, nil))
	got, err := s.Sent(context.Background(), "agent-a", ListOptions{Peer: "agent-b", Limit: json.RawMessage(`2`), Offset: json.RawMessage(`"1"`)})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(got)
	want := `{"messages":[{"id":"` + mailID + `","to_agent":"agent-b","subject":"S","body_preview":"prev","sent_at":"2026-11-05T12:00:00.123Z","acked_at":"2026-11-05T12:00:00.123Z","in_reply_to":null}]}`
	if string(out) != want {
		t.Errorf("got %s\nwant %s", out, want)
	}
}

func TestHistoryDefaults(t *testing.T) {
	s, mock := newService(t)
	expectActor(mock, "agent-b", 12)
	mock.ExpectQuery(`WHERE m.to_actor_id = \$1 AND m.acked_at IS NOT NULL AND m.deleted_at IS NULL\s+ORDER BY m.sent_at DESC\s+LIMIT \$2 OFFSET \$3`).
		WithArgs(12, jsutil.Number("50"), 0).
		WillReturnRows(pgxmock.NewRows([]string{"id", "from_agent", "subject", "body", "sent_at", "acked_at", "in_reply_to"}))
	got, err := s.History(context.Background(), "agent-b", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := json.Marshal(got); string(out) != `{"messages":[]}` {
		t.Errorf("got %s", out)
	}
}

// request builds a route request with an optional JSON body and agent.
func request(body, agent string) *http.Request {
	r := httptest.NewRequest("POST", "/v1/mail/x", nil)
	st := &httpapi.State{AuthenticatedAgent: agent}
	if body != "" {
		st.Body = json.RawMessage(body)
	}
	return httpapi.WithStateValue(r, st)
}

func TestIdentity(t *testing.T) {
	cases := []struct {
		body, agent string
		want        string
		forbidden   bool
		errText     string
	}{
		{`{}`, "agent-a", "agent-a", false, ""},
		{`{"agent":""}`, "agent-a", "agent-a", false, ""},
		{`{"agent":"!!!"}`, "agent-a", "agent-a", false, ""},
		{`{"agent":" Agent-A "}`, "agent-a", "agent-a", false, ""},
		{`{"agent":0}`, "agent-a", "agent-a", false, ""},
		{`{"agent":"agent-b"}`, "agent-a", "", true, ""},
		{`{"agent":5}`, "agent-a", "", true, ""},
		{`{"agent":["agent-a"]}`, "agent-a", "", true, ""},
		{`{"agent":"Agent-B"}`, "", "agent-b", false, ""},
		{`{}`, "", "", false, ""},
		{`{"agent":5}`, "", "", false, "agent must be a string"},
	}
	for _, c := range cases {
		r := request(c.body, c.agent)
		f, _ := httpapi.BodyFields(r)
		rec := httptest.NewRecorder()
		got, done, err := identity(rec, r, f, "agent", "agent does not match authenticated agent")
		switch {
		case c.forbidden:
			if !done || rec.Code != 403 || rec.Body.String() != `{"error":{"code":"IDENTITY_MISMATCH","message":"agent does not match authenticated agent"}}` {
				t.Errorf("%s as %q: done %v status %d body %s", c.body, c.agent, done, rec.Code, rec.Body)
			}
		case c.errText != "":
			if err == nil || err.Error() != c.errText {
				t.Errorf("%s as %q: err %v", c.body, c.agent, err)
			}
		default:
			if done || err != nil || got != c.want {
				t.Errorf("%s as %q: %q %v %v", c.body, c.agent, got, done, err)
			}
		}
	}
}

func TestNoBodyErrorsAreV8s(t *testing.T) {
	rt := &Routes{}
	cases := map[string]func(http.ResponseWriter, *http.Request) error{
		"Cannot destructure property 'in_reply_to' of 'req.body' as it is undefined.": rt.send,
		"Cannot destructure property 'id' of 'req.body' as it is undefined.":          rt.edit,
		"Cannot read properties of undefined (reading 'agent')":                       rt.check,
	}
	for want, fn := range cases {
		if err := fn(httptest.NewRecorder(), request("", "agent-a")); err == nil || err.Error() != want {
			t.Errorf("got %v, want %q", err, want)
		}
	}
	for _, fn := range []func(http.ResponseWriter, *http.Request) error{rt.receive, rt.ack, rt.sent, rt.history} {
		if err := fn(httptest.NewRecorder(), request("", "agent-a")); err == nil || err.Error() != "Cannot read properties of undefined (reading 'agent')" {
			t.Errorf("got %v", err)
		}
	}
	if err := rt.unsend(httptest.NewRecorder(), request("", "agent-a")); err == nil || err.Error() != "Cannot destructure property 'id' of 'req.body' as it is undefined." {
		t.Errorf("unsend: %v", err)
	}
}

func TestUnanswerableNotice(t *testing.T) {
	seam := virtualagent.Seam{}
	if got := seam.UnanswerableNotice("helper", "missing", []string{"model"}); got != "[Error] helper cannot reply: it has no model set. Its owner can add it in the dashboard (Agents → helper)." {
		t.Errorf("one: %q", got)
	}
	if got := seam.UnanswerableNotice("helper", "missing", []string{"model", "api key"}); got != "[Error] helper cannot reply: it has no model, api key set. Its owner can add them in the dashboard (Agents → helper)." {
		t.Errorf("two: %q", got)
	}
}
