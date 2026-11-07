package actors

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
)

// Availability is checkNameAvailability's answer; Reason is set only when
// the name is taken.
type Availability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// CheckNameAvailability reports whether name can be used for a new actor:
// no actor has it, and no note namespace (such as "shared") uses it.
func (c *Cache) CheckNameAvailability(ctx context.Context, conn db.DB, name string) (Availability, error) {
	_, found, err := c.ResolveByName(ctx, name)
	if err != nil {
		return Availability{}, err
	}
	if found {
		return Availability{Reason: "Name already taken."}, nil
	}
	var one int
	err = conn.QueryRow(ctx, "SELECT 1 FROM documents WHERE namespace = $1 LIMIT 1", name).Scan(&one)
	if err == nil {
		return Availability{Reason: namespaceConflict}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Availability{}, err
	}
	return Availability{Available: true}, nil
}

const namespaceConflict = "This name conflicts with an existing namespace."

// ClaimNamespace checks the namespace half of CheckNameAvailability again
// inside the transaction that inserts a new actor, just before the insert
// (LLM-931). The first check runs outside any transaction, and registration
// then spends the moderation call and the hashes before the insert. A note
// saved or moved into namespace name in that gap would belong to the new
// actor, which has its own namespace with no grant.
//
// SHARE mode conflicts with the ROW EXCLUSIVE lock every INSERT, UPDATE and
// DELETE on documents takes: the LOCK waits for in-flight writers to commit,
// the SELECT after it sees their rows, and no new row lands until tx ends,
// when the actor exists. lock_timeout bounds the wait, because every new note
// write queues behind a waiting LOCK.
func ClaimNamespace(ctx context.Context, tx pgx.Tx, name string) (Availability, error) {
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '5s'"); err != nil {
		return Availability{}, err
	}
	if _, err := tx.Exec(ctx, "LOCK TABLE documents IN SHARE MODE"); err != nil {
		return Availability{}, err
	}
	var one int
	err := tx.QueryRow(ctx, "SELECT 1 FROM documents WHERE namespace = $1 LIMIT 1", name).Scan(&one)
	if err == nil {
		return Availability{Reason: namespaceConflict}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Availability{}, err
	}
	return Availability{Available: true}, nil
}
