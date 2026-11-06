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
		return Availability{Reason: "This name conflicts with an existing namespace."}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Availability{}, err
	}
	return Availability{Available: true}, nil
}
