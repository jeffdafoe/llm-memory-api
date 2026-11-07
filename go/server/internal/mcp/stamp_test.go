package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
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
