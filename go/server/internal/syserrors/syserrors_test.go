package syserrors

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/system"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

func TestRecordWithoutAgent(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectExec("INSERT INTO system_errors").
		WithArgs((*int)(nil), "documents", "AUTO_INDEX_FAILED", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	rec := &Recorder{DB: mock}
	rec.Record(context.Background(), "", "documents", "AUTO_INDEX_FAILED", map[string]string{"error": "boom"})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordNilContextIsNull(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectExec("INSERT INTO system_errors").
		WithArgs((*int)(nil), "memory", "X", (*string)(nil)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	var logged string
	rec := &Recorder{DB: mock, Errorf: func(f string, a ...any) { logged = f }}
	rec.Record(context.Background(), "", "memory", "X", nil)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if logged != "" {
		t.Errorf("logged on success: %s", logged)
	}
}

// reportArgs are the insert's arguments (pgxmock drops pgjson's
// result-format argument).
func reportArgs(actorID *int, source, code string, details *string, status string, action *string, resolved bool) []any {
	resolvedArg := any((*time.Time)(nil))
	if resolved {
		resolvedArg = pgxmock.AnyArg()
	}
	return []any{actorID, source, code, details, status, action, resolvedArg}
}

func returned(id int, status string, action *string) *pgxmock.Rows {
	return pgxmock.NewRows([]string{"id", "status", "handler_action", "reported_at"}).
		AddRow(id, status, action, time.Date(2026, 11, 7, 16, 0, 0, 0, time.UTC))
}

func ptr[T any](v T) *T { return &v }

func parseObject(t *testing.T, raw string) *jsutil.Object {
	t.Helper()
	v, err := jsutil.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v.(*jsutil.Object)
}

func newRecorder(mock pgxmock.PgxPoolIface) *Recorder {
	cache := actors.NewCache(mock)
	return &Recorder{DB: mock, Actors: cache, Notifier: &system.Notifier{DB: mock, Actors: cache}}
}

func TestReportSubagentDeadSendsMessage(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.MatchExpectationsInOrder(true)
	message := "Your subagent appears to have died in discussion #1.5e+21. The transport is still running. Relaunch the subagent to continue."
	action := `Sent system message to agent-a: "` + message + `"`
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("system").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(1, "system"))
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("agent-a").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(2, "agent-a"))
	mock.ExpectQuery("INSERT INTO chat_message_texts").WithArgs(message, 1, nil).
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(10, time.Now()))
	mock.ExpectQuery("INSERT INTO chat_messages").WithArgs(10, 2).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(11))
	mock.ExpectQuery("INSERT INTO system_errors").
		WithArgs(reportArgs(ptr(2), "discuss-transport", "SUBAGENT_DEAD", ptr(`{"discussion_id":1.5e+21,"z":1}`), "auto_resolved", &action, true)...).
		WillReturnRows(returned(5, "auto_resolved", &action))

	row, err := newRecorder(mock).Report(context.Background(), "agent-a", "discuss-transport", "SUBAGENT_DEAD",
		parseObject(t, `{"discussion_id":1.50e21,"z":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if s, _ := row.String("status"); s != "auto_resolved" {
		t.Errorf("status %q", s)
	}
}

func TestReportSubagentDeadWithoutAgentFails(t *testing.T) {
	mock := testutil.NewMock(t)
	action := `Handler failed: Agent "undefined" is not registered`
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("system").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(1, "system"))
	mock.ExpectQuery("INSERT INTO system_errors").
		WithArgs(reportArgs(nil, "discuss-transport", "SUBAGENT_DEAD", nil, "unhandled", &action, false)...).
		WillReturnRows(returned(6, "unhandled", &action))

	if _, err := newRecorder(mock).Report(context.Background(), "", "discuss-transport", "SUBAGENT_DEAD", nil); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReportSubagentDeadUnconvertibleIDFails(t *testing.T) {
	mock := testutil.NewMock(t)
	action := "Handler failed: Cannot convert object to primitive value"
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("agent-a").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(2, "agent-a"))
	mock.ExpectQuery("INSERT INTO system_errors").
		WithArgs(reportArgs(ptr(2), "discuss-transport", "SUBAGENT_DEAD", ptr(`{"discussion_id":{"toString":1}}`), "unhandled", &action, false)...).
		WillReturnRows(returned(7, "unhandled", &action))

	if _, err := newRecorder(mock).Report(context.Background(), "agent-a", "discuss-transport", "SUBAGENT_DEAD",
		parseObject(t, `{"discussion_id":{"toString":1}}`)); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReportWithoutHandlerIsUnhandled(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("agent-a").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}))
	mock.ExpectQuery("INSERT INTO system_errors").
		WithArgs(reportArgs(nil, "discuss-transport", "subagent_dead", ptr(`{}`), "unhandled", nil, false)...).
		WillReturnRows(returned(8, "unhandled", nil))

	row, err := newRecorder(mock).Report(context.Background(), "agent-a", "discuss-transport", "subagent_dead", parseObject(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	// The mock gives no column types, so the JSON shape of the row is the
	// contract harness's to check.
	if s, _ := row.String("status"); s != "unhandled" {
		t.Errorf("status %q", s)
	}
}
