package testutil

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGURLVar names the variable that points the real-Postgres tests at a
// server. Its role must be able to create databases: each test gets its own.
const PGURLVar = "TEST_PG_URL"

// ScratchDB returns a pool on a new, empty database that is dropped when the
// test ends, after running schema in it. These tests check lock behavior a
// mock cannot show. Without PGURLVar they skip, so `go test ./...` works on
// a box with no Postgres. In CI (GitHub Actions sets CI=true) they fail
// instead, because a skip there would hide the guarantee they exist to
// check.
func ScratchDB(t *testing.T, schema string) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(PGURLVar)
	if url == "" {
		if os.Getenv("CI") == "true" {
			t.Fatalf("%s not set in CI", PGURLVar)
		}
		t.Skipf("%s not set", PGURLVar)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	// The nanosecond clock keeps parallel tests and packages apart: go test
	// runs packages as separate processes on the same server.
	name := fmt.Sprintf("scratch_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		admin.Close(ctx)
	})
	if _, err := pool.Exec(ctx, schema); err != nil {
		t.Fatal(err)
	}
	return pool
}

// WaitBlocked returns once some session in the scratch database waits on a
// lock (a row lock or an advisory lock). It fails the test if done closes
// first: the statement that should have waited went through.
func WaitBlocked(t *testing.T, pool *pgxpool.Pool, done <-chan struct{}, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("%s did not wait for the lock", what)
		default:
		}
		var waiting int
		err := pool.QueryRow(context.Background(),
			"SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'").Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s neither waited nor finished", what)
}
