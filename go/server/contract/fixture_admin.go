//go:build contract

package contract

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
)

// applyAdminFixture adds the limited admin the admin cases need for
// permission, ownership and delegation refusals: a dashboard password,
// dashboard:read, agents:write and notes:read but not *:*, a web session,
// and no realm. It created two actors: limited-agent (an agent with an API
// session, findable by the share search) and limited-va (a virtual agent).
// It runs last, so the actor ids the other fixtures create do not move.
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
	if _, err := tx.Exec(ctx,
		`INSERT INTO admin_permissions (actor_id, resource, action)
         VALUES ($1, 'dashboard', 'read'), ($1, 'agents', 'write'), ($1, 'notes', 'read')`, id); err != nil {
		return err
	}
	if err := insertSession(ctx, tx, id, creds["admin-limited-web"], "limited admin web session", "web"); err != nil {
		return err
	}

	var agentID int
	if err := tx.QueryRow(ctx,
		"INSERT INTO actors (name, created_by, visible_to_others) VALUES ($1, $2, true) RETURNING id",
		limitedAgentName, id).Scan(&agentID); err != nil {
		return fmt.Errorf("limited agent: %w", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO agent_configuration (actor_id) VALUES ($1)", agentID); err != nil {
		return err
	}
	if err := insertSession(ctx, tx, agentID, creds["limited-agent-session"], "limited agent api session", "api"); err != nil {
		return err
	}

	var vaID int
	if err := tx.QueryRow(ctx,
		"INSERT INTO actors (name, created_by, status) VALUES ($1, $2, 'available') RETURNING id",
		limitedVAName, id).Scan(&vaID); err != nil {
		return fmt.Errorf("limited va: %w", err)
	}
	_, err = tx.Exec(ctx, "INSERT INTO agent_configuration (actor_id, virtual, provider, model) VALUES ($1, true, 'anthropic', 'claude-haiku-4-5')", vaID)
	return err
}
