package activity

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
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
	cancel()
	if !(&Sweeper{DB: mock, Events: &recordedEvents{}, Errorf: t.Logf}).Run(ctx, time.Second) {
		t.Fatal("Run reported sweeps left running")
	}
	// No sweep runs before the first interval has passed.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// blockingDB holds every query until its context ends, counting how many
// are in flight at once.
type blockingDB struct {
	db.DB
	mu       sync.Mutex
	inFlight int
	most     int
	started  chan struct{}
}

func (b *blockingDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	b.mu.Lock()
	b.inFlight++
	b.most = max(b.most, b.inFlight)
	b.mu.Unlock()
	b.started <- struct{}{}
	<-ctx.Done()
	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
	return nil, ctx.Err()
}

// As with Node's setInterval, a sweep blocked on a slow statement does not
// hold back the next one, and has no deadline of its own. Shutdown waits for
// the blocked sweeps, then cancels them.
func TestBlockedSweepsOverlapAndDrain(t *testing.T) {
	bdb := &blockingDB{started: make(chan struct{}, 100)}
	var errMu sync.Mutex
	var errs []string
	s := &Sweeper{DB: bdb, Events: &recordedEvents{}, Every: 10 * time.Millisecond, Errorf: func(format string, args ...any) {
		errMu.Lock()
		errs = append(errs, fmt.Sprintf(format, args...))
		errMu.Unlock()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan bool)
	go func() { result <- s.Run(ctx, 50*time.Millisecond) }()
	for range 3 {
		select {
		case <-bdb.started:
		case <-time.After(2 * time.Second):
			t.Fatal("sweeps did not start")
		}
	}
	start := time.Now()
	cancel()
	if !<-result {
		t.Fatal("cancelled sweeps did not return")
	}
	if waited := time.Since(start); waited < 50*time.Millisecond {
		t.Fatalf("Run cancelled the sweeps after %s, before its drain wait", waited)
	}
	bdb.mu.Lock()
	most, left := bdb.most, bdb.inFlight
	bdb.mu.Unlock()
	if most < 3 || left != 0 {
		t.Fatalf("at most %d sweeps at once, %d left", most, left)
	}
	errMu.Lock()
	defer errMu.Unlock()
	if len(errs) < 3 || errs[0] != "Stale activity cleanup error: context canceled" {
		t.Fatalf("errors %v", errs)
	}
}

// A tick that is ready together with the cancel must not start a sweep.
// select chooses at random between ready cases, so this runs many times.
func TestNoSweepAfterCancel(t *testing.T) {
	bdb := &blockingDB{started: make(chan struct{}, 100)}
	for range 200 {
		ticks := make(chan time.Time, 1)
		ticks <- time.Now()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s := &Sweeper{DB: bdb, Events: &recordedEvents{}, Errorf: t.Logf, ticks: ticks}
		if !s.Run(ctx, time.Second) {
			t.Fatal("Run reported sweeps left running")
		}
	}
	if n := len(bdb.started); n != 0 {
		t.Fatalf("%d sweeps started after the cancel", n)
	}
}
