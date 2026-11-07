package actors

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests run ClaimNamespace against a real Postgres, because what they
// check is lock behavior a mock cannot show (LLM-931). CONTRACT_PG_URL is a
// URL whose role may create databases (the contract harness's variable);
// without it they skip.

func scratchDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("CONTRACT_PG_URL")
	if url == "" {
		t.Skip("CONTRACT_PG_URL not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("actors_claim_%d", time.Now().UnixNano())
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
	if _, err := pool.Exec(ctx, "CREATE TABLE documents (id serial PRIMARY KEY, namespace text NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	return pool
}

// waitBlocked returns once some session in the scratch database waits on a
// lock. It fails the test if done closes first: the statement that should
// have waited went through.
func waitBlocked(t *testing.T, pool *pgxpool.Pool, done <-chan struct{}, what string) {
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

// A note save still in flight when the claim starts: the claim waits for it
// to commit and then sees the note.
func TestClaimNamespaceWaitsForASaveInFlight(t *testing.T) {
	pool := scratchDB(t)
	ctx := context.Background()

	save, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer save.Rollback(ctx)
	if _, err := save.Exec(ctx, "INSERT INTO documents (namespace) VALUES ('newcomer')"); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var claim Availability
	var claimErr error
	go func() {
		defer close(done)
		tx, err := pool.Begin(ctx)
		if err != nil {
			claimErr = err
			return
		}
		defer tx.Rollback(ctx)
		claim, claimErr = ClaimNamespace(ctx, tx, "newcomer")
	}()

	waitBlocked(t, pool, done, "the claim")
	if err := save.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	<-done
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	if claim.Available || claim.Reason != namespaceConflict {
		t.Fatalf("claim after the save committed: %+v", claim)
	}
}

// A note save that starts after the claim: it waits until the actor's
// transaction ends, so it cannot land in the namespace before the actor
// exists.
func TestClaimNamespaceHoldsOffALaterSave(t *testing.T) {
	pool := scratchDB(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	claim, err := ClaimNamespace(ctx, tx, "newcomer")
	if err != nil {
		t.Fatal(err)
	}
	if !claim.Available {
		t.Fatalf("claim on an empty namespace: %+v", claim)
	}

	done := make(chan struct{})
	var saveErr error
	go func() {
		defer close(done)
		_, saveErr = pool.Exec(ctx, "INSERT INTO documents (namespace) VALUES ('newcomer')")
	}()

	waitBlocked(t, pool, done, "the save")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	<-done
	if saveErr != nil {
		t.Fatal(saveErr)
	}
}
