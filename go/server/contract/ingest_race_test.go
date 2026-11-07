//go:build contract

package contract

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// raceIngests is how many ingests of one file each server gets at once.
const raceIngests = 12

var raceVersion = regexp.MustCompile(`version (\d+)`)

// Many ingests of one source file at once must leave exactly one version's
// chunks (LLM-919). Without the per-file lock each ingest's DELETE can run
// before the others insert, and several versions stay. The scenarios
// cannot test this: their steps run one after another. This runs on the
// first shard only, as each server needs its own start.
func TestIngestConcurrentKeepsOneVersion(t *testing.T) {
	index, _, err := parseShard(os.Getenv("CONTRACT_SHARD"))
	if err != nil {
		t.Fatal(err)
	}
	if index != 1 {
		t.Skip("runs on the first shard")
	}
	for _, side := range []string{"node", "go"} {
		t.Run(side, func(t *testing.T) {
			t.Parallel()
			raceIngestOneSide(t, side)
		})
	}
}

func raceIngestOneSide(t *testing.T, side string) {
	// A stuck ingest fails the test with the server's output, instead of
	// hanging until the package timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	fake := NewFakeUpstream()
	t.Cleanup(fake.Close)
	db, err := harness.cluster.Copy(ctx, side)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := db.Drop(dropCtx); err != nil {
			t.Logf("drop %s: %v", db.Name, err)
		}
	})
	proc, err := StartServer(ctx, serverSpec(side, db, t.TempDir()), fake)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proc.Stop)
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("%s\n--- %s output ---\n%s", fmt.Sprintf(format, args...), side, tail(proc.Output(), 3000))
	}

	token := Credentials()["agent-a-session"]
	errs := make([]error, raceIngests)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range raceIngests {
		done.Add(1)
		go func() {
			defer done.Done()
			// The lock key lowercases the source file, as the DELETE does.
			file := "race/doc"
			if i%2 == 1 {
				file = "RACE/Doc"
			}
			start.Wait()
			errs[i] = postIngest(ctx, proc.BaseURL, token, raceBody(file, strconv.Itoa(i)))
		}()
	}
	start.Done()
	done.Wait()
	for i, err := range errs {
		if err != nil {
			fail("ingest %d: %v", i, err)
		}
	}

	conn, err := pgx.Connect(ctx, harness.cluster.URL(db.Name, false))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	texts := raceChunks(t, ctx, conn)
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

	// The burst can pass by luck. This cannot: hold the file's lock from
	// outside, and an ingest must wait on it until the holder commits.
	holder, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_xact_lock(919001, hashtext('agent-a' || '/' || LOWER('RACE/Doc')))"); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		result <- postIngest(ctx, proc.BaseURL, token, raceBody("Race/DOC", "held"))
	}()
	for {
		var waiting bool
		err := holder.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
			WHERE locktype = 'advisory' AND classid = 919001 AND NOT granted
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database()))`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-result:
			fail("the ingest finished while the file's lock was held (err %v)", err)
		case <-ctx.Done():
			fail("the ingest never waited on the file's lock")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			fail("ingest after the lock: %v", err)
		}
	case <-ctx.Done():
		fail("the ingest did not finish after the lock was released")
	}
	texts = raceChunks(t, ctx, conn)
	if len(texts) != 2 || !strings.Contains(texts[0], "version held") || !strings.Contains(texts[1], "version held") {
		t.Errorf("after the held ingest: %q, want its 2 chunks only", texts)
	}
}

// raceBody is an ingest of two chunks, each tagged with the version.
func raceBody(file, version string) string {
	return fmt.Sprintf(`{"namespace":"agent-a","source_file":%q,"content":"# Part one\n\nversion %s, part one.\n\n# Part two\n\nversion %s, part two."}`, file, version, version)
}

// raceChunks reads the test file's chunks.
func raceChunks(t *testing.T, ctx context.Context, conn *pgx.Conn) []string {
	t.Helper()
	rows, err := conn.Query(ctx, "SELECT chunk_text FROM memory_chunks WHERE namespace = 'agent-a' AND LOWER(source_file) = 'race/doc' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return texts
}

func postIngest(ctx context.Context, baseURL, token, body string) error {
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/v1/memory/ingest", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, out)
	}
	return nil
}
