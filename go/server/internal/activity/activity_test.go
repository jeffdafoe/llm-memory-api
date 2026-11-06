package activity

import (
	"context"
	"regexp"
	"sync"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

type recordedEvents struct {
	mu   sync.Mutex
	sent []string
}

func (e *recordedEvents) Broadcast(event string, data any) {
	b, _ := jsonutil.Marshal(data)
	e.mu.Lock()
	e.sent = append(e.sent, event+" "+string(b))
	e.mu.Unlock()
}

// The statement is server.js's, character for character.
const nodeSQL = "UPDATE actors SET active_since = NULL\n" +
	"                 WHERE active_since IS NOT NULL\n" +
	"                   AND last_seen < NOW() - INTERVAL '5 minutes'\n" +
	"                 RETURNING id, name"

func TestSweepClearsAndBroadcasts(t *testing.T) {
	if sweepSQL != nodeSQL {
		t.Fatalf("sweep SQL differs from Node's:\n%s", sweepSQL)
	}
	mock := testutil.NewMock(t)
	mock.ExpectQuery(regexp.QuoteMeta(nodeSQL)).WillReturnRows(
		pgxmock.NewRows([]string{"id", "name"}).AddRow(int64(3), "agent-a").AddRow(int64(9), "agent-b"))
	ev := &recordedEvents{}
	s := &Sweeper{DB: mock, Events: ev, Errorf: t.Logf}
	if err := s.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	want := []string{`agent_activity {"agent":"agent-a","active":false}`, `agent_activity {"agent":"agent-b","active":false}`}
	if len(ev.sent) != 2 || ev.sent[0] != want[0] || ev.sent[1] != want[1] {
		t.Fatalf("events %v", ev.sent)
	}
}

func TestRunStopsWithContext(t *testing.T) {
	mock := testutil.NewMock(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		(&Sweeper{DB: mock, Events: &recordedEvents{}, Errorf: t.Logf}).Run(ctx)
		close(done)
	}()
	cancel()
	<-done
	// No sweep runs before the first interval has passed.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
