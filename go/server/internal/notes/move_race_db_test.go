package notes

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// pausingDB passes every call through, but the first statement containing
// pauseOn waits until release is closed, after closing reached. It wraps
// transactions too, so the pause works whether the code under test runs the
// statement on the pool or inside a transaction.
type pausingDB struct {
	db.DB
	pauseOn  string
	reached  chan struct{}
	release  chan struct{}
	pausedAt sync.Once
	released sync.Once
}

// Release lets the paused statement run. It is safe to call more than once,
// so a test can defer it and still release early.
func (p *pausingDB) Release() {
	p.released.Do(func() { close(p.release) })
}

func newPausingDB(inner db.DB, pauseOn string) *pausingDB {
	return &pausingDB{DB: inner, pauseOn: pauseOn, reached: make(chan struct{}), release: make(chan struct{})}
}

func (p *pausingDB) pause(sql string) {
	if strings.Contains(sql, p.pauseOn) {
		p.pausedAt.Do(func() {
			close(p.reached)
			<-p.release
		})
	}
}

func (p *pausingDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	p.pause(sql)
	return p.DB.Exec(ctx, sql, args...)
}

func (p *pausingDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return pausingTx{Tx: tx, p: p}, nil
}

type pausingTx struct {
	pgx.Tx
	p *pausingDB
}

func (t pausingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.p.pause(sql)
	return t.Tx.Exec(ctx, sql, args...)
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not happen", what)
	}
}

// waitDoneOrBlocked returns once done closes or some session in the scratch
// database waits on a lock.
func waitDoneOrBlocked(t *testing.T, pool *pgxpool.Pool, done <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			return
		default:
		}
		var waiting int
		if err := pool.QueryRow(context.Background(),
			"SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the second move neither finished nor waited")
}

func chunkSlugs(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), "SELECT DISTINCT source_file FROM memory_chunks ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	slugs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return slugs
}

// Move a → b pauses after moving the note and before moving its chunks;
// move b → c runs in that gap. The chunks must end up wherever the note
// ends up. As two separate writes, the second move moved the note to c
// with no chunks at b to take, and the first then left the chunks at b.
func TestMoveKeepsChunksWithTheNoteUnderAConcurrentMove(t *testing.T) {
	pool := testutil.ScratchDB(t, deletedCopiesSchema)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "INSERT INTO documents (namespace, slug, content) VALUES ('ns', 'notes/a', 'text')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO memory_chunks (namespace, source_file, chunk_text) VALUES ('ns', 'notes/a', 'one'), ('ns', 'notes/a', 'two')"); err != nil {
		t.Fatal(err)
	}

	first := newDeletedCopiesService(pool)
	paused := newPausingDB(pool, "UPDATE memory_chunks")
	first.DB = paused
	second := newDeletedCopiesService(pool)

	// Deferred before the first move starts: a failed assertion below
	// must not leave that move paused with its transaction open while the
	// scratch database is dropped.
	defer paused.Release()
	firstDone := make(chan struct{})
	var firstErr error
	go func() {
		defer close(firstDone)
		_, firstErr = first.Move(ctx, MoveInput{Namespace: "ns", Slug: "notes/a", NewSlug: "notes/b"})
	}()
	waitClosed(t, paused.reached, "the first move reaching its chunk update")

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		// Its result depends on the ordering (moved, or 404 because the
		// note is not yet at b for anyone else); only the end state counts.
		second.Move(ctx, MoveInput{Namespace: "ns", Slug: "notes/b", NewSlug: "notes/c"})
	}()
	waitDoneOrBlocked(t, pool, secondDone)
	paused.Release()
	waitClosed(t, firstDone, "the first move finishing")
	waitClosed(t, secondDone, "the second move finishing")
	if firstErr != nil {
		t.Fatalf("first move: %v", firstErr)
	}

	var noteSlug string
	if err := pool.QueryRow(ctx, "SELECT slug FROM documents WHERE deleted_at IS NULL").Scan(&noteSlug); err != nil {
		t.Fatal(err)
	}
	if slugs := chunkSlugs(t, pool); len(slugs) != 1 || slugs[0] != noteSlug {
		t.Errorf("the note is at %s but its chunks are at %q", noteSlug, slugs)
	}
}

// An edit's background re-index reaches the live row it edited: the chunks
// appear with the new text. (IngestDocument's own tests cover the re-index
// losing to a delete, move or later edit.)
func TestEditReindexesTheLiveNote(t *testing.T) {
	pool := testutil.ScratchDB(t, deletedCopiesSchema)
	seedNoteWithDeletedCopies(t, pool)
	s := newDeletedCopiesService(pool)

	newString := "VERSION THREE"
	if _, err := s.Edit(context.Background(), EditInput{Namespace: "ns", Slug: "notes/doc", OldString: "version three", NewString: &newString}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var text string
		err := pool.QueryRow(context.Background(), "SELECT chunk_text FROM memory_chunks WHERE namespace = 'ns' AND source_file = 'notes/doc'").Scan(&text)
		if err == nil {
			if text != "VERSION THREE, live" {
				t.Fatalf("chunk text = %q", text)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the edit's re-index wrote no chunks")
}
