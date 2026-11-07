package syserrors

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/system"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// hangUpDB cancels the request (the client hangs up) once the system
// message's delivery row is written, before the system_errors insert.
type hangUpDB struct {
	pgxmock.PgxPoolIface
	cancel context.CancelFunc
}

func (h hangUpDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := h.PgxPoolIface.QueryRow(ctx, sql, args...)
	if strings.HasPrefix(sql, "INSERT INTO chat_messages") {
		h.cancel()
	}
	return row
}

// A client that hangs up after the handler's message cannot stop the row:
// Node's queries ran on after a disconnect.
func TestReportSurvivesHangUp(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.MatchExpectationsInOrder(true)
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("system").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(1, "system"))
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("agent-a").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(2, "agent-a"))
	mock.ExpectQuery("INSERT INTO chat_message_texts").WithArgs(pgxmock.AnyArg(), 1, nil).
		WillReturnRows(pgxmock.NewRows([]string{"id", "sent_at"}).AddRow(10, testTime))
	mock.ExpectQuery("INSERT INTO chat_messages").WithArgs(10, 2).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(11))
	mock.ExpectQuery("INSERT INTO system_errors").
		WithArgs(ptr(2), "discuss-transport", "SUBAGENT_DEAD", ptr(`{"discussion_id":4}`), "auto_resolved", pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnRows(returned(5, "auto_resolved", ptr("Sent")))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := hangUpDB{PgxPoolIface: mock, cancel: cancel}
	cache := actors.NewCache(conn)
	log := logger.New(conn, cache, "")
	rt := &Routes{
		Recorder: &Recorder{DB: conn, Actors: cache, Notifier: &system.Notifier{DB: conn, Actors: cache}},
		Router:   &httpapi.Router{Log: log},
		Log:      log,
	}
	r := httptest.NewRequest("POST", "/v1/system/error/report", nil).WithContext(ctx)
	r = httpapi.WithStateValue(r, &httpapi.State{AuthenticatedAgent: "agent-a", ActorID: 2,
		Body: json.RawMessage(`{"source":"discuss-transport","error_code":"SUBAGENT_DEAD","context":{"discussion_id":4}}`)})
	w := httptest.NewRecorder()
	if err := rt.report(w, r); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("the request was not cancelled")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
	if w.Code != 200 {
		t.Errorf("status %d: %s", w.Code, w.Body)
	}
}
