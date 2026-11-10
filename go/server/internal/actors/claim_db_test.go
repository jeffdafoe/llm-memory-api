package actors

import (
	"context"
	"testing"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// These tests run ClaimNamespace against a real Postgres, because what they
// check is lock behavior a mock cannot show (LLM-931). testutil.ScratchDB
// says when they run.

// claimSchema is the one table ClaimNamespace reads and locks.
const claimSchema = "CREATE TABLE documents (id serial PRIMARY KEY, namespace text NOT NULL)"

// A note save still in flight when the claim starts: the claim waits for it
// to commit and then sees the note.
func TestClaimNamespaceWaitsForASaveInFlight(t *testing.T) {
	pool := testutil.ScratchDB(t, claimSchema)
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

	testutil.WaitBlocked(t, pool, done, "the claim")
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
	pool := testutil.ScratchDB(t, claimSchema)
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

	testutil.WaitBlocked(t, pool, done, "the save")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	<-done
	if saveErr != nil {
		t.Fatal(saveErr)
	}
}
