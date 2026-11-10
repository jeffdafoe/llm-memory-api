package memory

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// These tests run Ingest against a real Postgres, because the per-file lock
// (LLM-919) is behavior a mock cannot show. testutil.ScratchDB says when they
// run.

// chunksSchema has the memory_chunks columns Ingest writes. The embedding is
// text, not pgvector's vector: the lock does not depend on the type, and
// plain Postgres can then run the tests.
const chunksSchema = `CREATE TABLE memory_chunks (
	id serial PRIMARY KEY,
	namespace text NOT NULL,
	source_file text NOT NULL,
	heading text,
	chunk_text text NOT NULL,
	embedding text
)`

// raceIngests is how many ingests of one file run at once.
const raceIngests = 12

var raceVersion = regexp.MustCompile(`version (\S+),`)

// raceContent is a document of two chunks, each tagged with the version.
func raceContent(version string) string {
	return fmt.Sprintf("# Part one\n\nversion %s, part one.\n\n# Part two\n\nversion %s, part two.", version, version)
}

// raceChunks reads the chunks of the test file, in any letter case.
func raceChunks(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"SELECT chunk_text FROM memory_chunks WHERE namespace = 'agent-a' AND LOWER(source_file) = 'race/doc' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return texts
}

// Many ingests of one source file at once must leave exactly one version's
// chunks. Without the lock each ingest's DELETE can run before the others
// insert, and several versions stay. The file name alternates case because
// the lock key lowercases it, as the DELETE does.
func TestIngestConcurrentKeepsOneVersion(t *testing.T) {
	pool := testutil.ScratchDB(t, chunksSchema)
	s := &Service{DB: pool, Embedder: fixedEmbedder{}}

	errs := make([]error, raceIngests)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range raceIngests {
		done.Add(1)
		go func() {
			defer done.Done()
			file := "race/doc"
			if i%2 == 1 {
				file = "RACE/Doc"
			}
			// Every goroutine waits here, so the ingests start together.
			start.Wait()
			_, errs[i] = s.Ingest(context.Background(), "agent-a", file, raceContent(strconv.Itoa(i)))
		}()
	}
	start.Done()
	done.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("ingest %d: %v", i, err)
		}
	}

	texts := raceChunks(t, pool)
	versions := map[string]bool{}
	for _, text := range texts {
		m := raceVersion.FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("chunk without a version: %q", text)
		}
		versions[m[1]] = true
	}
	if len(texts) != 2 || len(versions) != 1 {
		t.Errorf("%d chunks from %d versions after %d ingests at once, want 2 chunks from 1 version: %q", len(texts), len(versions), raceIngests, texts)
	}
}

// The burst above can pass by luck. This cannot: hold the file's lock from
// outside, and an ingest must wait on it until the holder commits, then
// replace the old version.
func TestIngestWaitsForTheFileLock(t *testing.T) {
	pool := testutil.ScratchDB(t, chunksSchema)
	s := &Service{DB: pool, Embedder: fixedEmbedder{}}
	ctx := context.Background()

	if _, err := s.Ingest(ctx, "agent-a", "race/doc", raceContent("old")); err != nil {
		t.Fatal(err)
	}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx)
	// The same key Ingest takes, built from a third spelling of the name.
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_xact_lock($1, hashtext('agent-a' || '/' || LOWER('RACE/Doc')))", ingestLockClass); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var ingestErr error
	go func() {
		defer close(done)
		_, ingestErr = s.Ingest(ctx, "agent-a", "Race/DOC", raceContent("held"))
	}()

	testutil.WaitBlocked(t, pool, done, "the ingest")
	if got := raceChunks(t, pool); len(got) != 2 || !strings.Contains(got[0], "version old,") {
		t.Fatalf("while the lock is held: %q, want the old version's 2 chunks", got)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	<-done
	if ingestErr != nil {
		t.Fatal(ingestErr)
	}
	got := raceChunks(t, pool)
	if len(got) != 2 || !strings.Contains(got[0], "version held,") || !strings.Contains(got[1], "version held,") {
		t.Errorf("after the held ingest: %q, want its 2 chunks only", got)
	}
}
