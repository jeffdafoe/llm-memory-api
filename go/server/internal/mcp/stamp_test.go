package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
)

const stampSQL = "SET first_tool_call_at"

func stampServer(t *testing.T) (*Server, pgxmock.PgxPoolIface) {
	s, mock, _ := testServer(t)
	s.Config = config.NewStore()
	s.Config.Set("signup_funnel_since", "2026-10-01T00:00:00.000000Z")
	return s, mock
}

// The stamp is written before the tool runs and before callTool returns
// (LLM-954): a slow stamp holds the call, it does not land after it.
func TestToolCallStampFinishesFirst(t *testing.T) {
	s, mock := stampServer(t)
	mock.MatchExpectationsInOrder(true)
	mock.ExpectExec(stampSQL).WithArgs(7, pgxmock.AnyArg()).
		WillDelayFor(100 * time.Millisecond).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec("SET active_since = NULL").WithArgs(7).WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	c := caller{agent: "agent-a", actorID: 7, permissions: []string{"mcp_agent_status"}}
	res, err := s.callTool(context.Background(), c, "activity_stop", argsOf(t, `{}`))
	if err != nil || res.isError {
		t.Fatalf("activity_stop: %+v, %v", res, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A failed stamp is logged and the tool still runs, as Node's caught error
// let it.
func TestToolCallStampFailureStillRunsTool(t *testing.T) {
	s, mock := stampServer(t)
	mock.MatchExpectationsInOrder(true)
	mock.ExpectExec(stampSQL).WithArgs(7, pgxmock.AnyArg()).WillReturnError(errors.New("simulated stamp failure"))
	mock.ExpectExec("SET active_since = NULL").WithArgs(7).WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	c := caller{agent: "agent-a", actorID: 7, permissions: []string{"mcp_agent_status"}}
	res, err := s.callTool(context.Background(), c, "activity_stop", argsOf(t, `{}`))
	if err != nil || res.isError || res.text != "Activity stopped for agent-a." {
		t.Fatalf("activity_stop: %+v, %v", res, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// blockingStampDB holds the stamp UPDATE until release is closed, then
// records its context's error and runs it.
type blockingStampDB struct {
	pgxmock.PgxPoolIface
	started, release chan struct{}
	stampErr         chan error
}

func (d *blockingStampDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, stampSQL) {
		close(d.started)
		<-d.release
		d.stampErr <- ctx.Err()
	}
	return d.PgxPoolIface.Exec(ctx, sql, args...)
}

// A client that goes away while the stamp is running stops neither the
// stamp nor the tool: both run on the context detached sets up.
func TestToolCallStampOutlivesTheClient(t *testing.T) {
	s, mock := stampServer(t)
	mock.MatchExpectationsInOrder(true)
	mock.ExpectExec(stampSQL).WithArgs(7, pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec("SET active_since = NULL").WithArgs(7).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	db := &blockingStampDB{PgxPoolIface: mock, started: make(chan struct{}), release: make(chan struct{}), stampErr: make(chan error, 1)}
	s.DB = db
	workersDone := make(chan struct{})
	s.callsDone = func() { close(workersDone) }

	ctx, disconnect := context.WithCancel(context.Background())
	body := `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"activity_stop","arguments":{}}}]`
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body)).WithContext(ctx)
	for k, v := range legacy {
		r.Header.Set(k, v)
	}
	w := &countingWriter{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			st := httpapi.StateOf(r)
			st.MCPAgent, st.MCPActorID, st.MCPPermissions = "agent-a", 7, []string{"mcp_agent_status"}
			s.ServeHTTP(w, r)
		})
		httpapi.WithState(httpapi.ParseJSONBody(inner)).ServeHTTP(w, r)
	}()

	<-db.started
	disconnect()
	<-done
	w.closed.Store(true)
	close(db.release)
	if err := <-db.stampErr; err != nil {
		t.Fatalf("the stamp's context was cancelled: %v", err)
	}
	<-workersDone
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// stampCtxDB records the deadline of the context each stamp ran with.
type stampCtxDB struct {
	pgxmock.PgxPoolIface
	deadlines []time.Time
}

func (d *stampCtxDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.Contains(sql, stampSQL) {
		deadline, _ := ctx.Deadline()
		d.deadlines = append(d.deadlines, deadline)
	}
	return d.PgxPoolIface.Exec(ctx, sql, args...)
}

// The stamp runs on the tool call's own context: the one detached sets up,
// already free of the client's cancellation and bounded by toolTimeout. It
// adds no deadline of its own.
func TestToolCallStampUsesToolContext(t *testing.T) {
	s, mock := stampServer(t)
	rec := &stampCtxDB{PgxPoolIface: mock}
	s.DB = rec
	mock.ExpectExec(stampSQL).WithArgs(7, pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec("SET active_since = NULL").WithArgs(7).WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	deadline := time.Now().Add(toolTimeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	c := caller{agent: "agent-a", actorID: 7, permissions: []string{"mcp_agent_status"}}
	if _, err := s.callTool(ctx, c, "activity_stop", argsOf(t, `{}`)); err != nil {
		t.Fatalf("activity_stop: %v", err)
	}
	if len(rec.deadlines) != 1 || !rec.deadlines[0].Equal(deadline) {
		t.Fatalf("stamp deadlines: %v, want %v", rec.deadlines, deadline)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
