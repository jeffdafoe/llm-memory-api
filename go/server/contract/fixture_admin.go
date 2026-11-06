//go:build contract

package contract

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
)

// applyAdminFixture adds the limited admin the admin cases need for
// permission refusals: a dashboard password, only dashboard:read, a web
// session, and no realm. It runs last, so the actor ids the other fixtures
// create do not move.
func applyAdminFixture(ctx context.Context, tx pgx.Tx) error {
	creds := Credentials()
	salt := fixedSalt("limited admin password")
	h, err := auth.Hash(limitedAdminPassword, salt)
	if err != nil {
		return err
	}
	var id int
	if err := tx.QueryRow(ctx,
		"INSERT INTO actors (name, password_hash, password_salt) VALUES ($1, $2, $3) RETURNING id",
		LimitedAdminName, h, salt).Scan(&id); err != nil {
		return fmt.Errorf("limited admin: %w", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO admin_permissions (actor_id, resource, action) VALUES ($1, 'dashboard', 'read')", id); err != nil {
		return err
	}
	return insertSession(ctx, tx, id, creds["admin-limited-web"], "limited admin web session", "web")
}
