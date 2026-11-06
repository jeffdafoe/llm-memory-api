//go:build contract

package contract

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Busy must see a connection of the servers' app role that holds a
// transaction open, and stop counting it once the transaction ends.
func TestScenarioDBBusySeesAppRoleTransaction(t *testing.T) {
	ctx := context.Background()
	db, err := harness.cluster.Copy(ctx, "busy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Drop(context.Background()); err != nil {
			t.Logf("drop %s: %v", db.Name, err)
		}
	})

	app, err := pgx.Connect(ctx, harness.cluster.URL(db.Name, true))
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close(ctx)

	if n, err := db.Busy(ctx); err != nil || n != 0 {
		t.Fatalf("idle app connection: Busy = %d, %v; want 0", n, err)
	}
	tx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if n, err := db.Busy(ctx); err != nil || n != 1 {
		t.Fatalf("open transaction: Busy = %d, %v; want 1", n, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := db.Busy(ctx); err != nil || n != 0 {
		t.Fatalf("after rollback: Busy = %d, %v; want 0", n, err)
	}
}
