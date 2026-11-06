//go:build contract

package contract

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

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
	ctx := context.Background()
	fake := NewFakeUpstream()
	t.Cleanup(fake.Close)
	db, err := harness.cluster.Copy(ctx, side)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Drop(context.Background()); err != nil {
			t.Logf("drop %s: %v", db.Name, err)
		}
	})
	proc, err := StartServer(ctx, serverSpec(side, db, t.TempDir()), fake)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proc.Stop)

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
			body := fmt.Sprintf(`{"namespace":"agent-a","source_file":%q,"content":"# Part one\n\nversion %d, part one.\n\n# Part two\n\nversion %d, part two."}`, file, i, i)
			start.Wait()
			errs[i] = postIngest(ctx, proc.BaseURL, token, body)
		}()
	}
	start.Done()
	done.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("ingest %d: %v\n--- %s output ---\n%s", i, err, side, tail(proc.Output(), 3000))
		}
	}

	conn, err := pgx.Connect(ctx, harness.cluster.URL(db.Name, false))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	rows, err := conn.Query(ctx, "SELECT chunk_text FROM memory_chunks WHERE namespace = 'agent-a' AND LOWER(source_file) = 'race/doc'")
	if err != nil {
		t.Fatal(err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
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
