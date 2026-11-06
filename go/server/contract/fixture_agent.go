//go:build contract

package contract

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// fixtureSoul is agent-b's context/soul note, which /agent/instructions/read
// appends. Its text matches no grep or search case.
const fixtureSoul = "Agent B keeps a steady watch."

// applyAgentFixture adds what the /v1/agent routes read:
//   - realm "contract" for the admin, agent-a and agent-b (helper-va keeps
//     none, so it is outside every realm);
//   - visibility: agent-a is granted agent-b and helper-va (the second is
//     outside its realm, so it does not count), agent-b has a wildcard row;
//   - agent-a: startup instructions, expertise, a sync mapping, and the
//     subsystem "contract-sync" on its fixture API session;
//   - agent-b: dream mode "companion" (the dream bootstrap is appended),
//     provider and model, and a context/soul note (no chunks, so search
//     never sees it).
func applyAgentFixture(ctx context.Context, tx pgx.Tx, adminID int, agents map[string]int) error {
	aID, bID := agents[AgentAName], agents[AgentBName]
	var helperID int
	if err := tx.QueryRow(ctx, "SELECT id FROM actors WHERE name = 'helper-va'").Scan(&helperID); err != nil {
		return err
	}
	stmts := []struct {
		sql  string
		args []any
	}{
		{"UPDATE actors SET realms = '{contract}' WHERE id = ANY($1)", []any{[]int{adminID, aID, bID}}},
		{"INSERT INTO actor_visibility_configuration (actor_id, target_actor_id) VALUES ($1, $2), ($1, $3), ($4, NULL)", []any{aID, bID, helperID, bID}},
		{`UPDATE agent_configuration SET startup_instructions = 'Agent A instructions.' WHERE actor_id = $1`, []any{aID}},
		{`UPDATE actors SET expertise = '["go","postgres"]' WHERE id = $1`, []any{aID}},
		{`INSERT INTO note_synchronization (actor_id, namespace, slug, local_path, created_at)
          VALUES ($1, 'agent-a', 'notes/', 'C:/sync/notes', '2026-10-01T09:00:00Z'),
                 ($1, 'shared', 'notes/codebase/', 'C:/sync/codebase', '2026-10-01T09:00:00Z')`, []any{aID}},
		{"UPDATE sessions SET subsystem = 'contract-sync' WHERE actor_id = $1 AND kind = 'api'", []any{aID}},
		{`UPDATE agent_configuration SET dream_mode = 'companion', provider = 'anthropic', model = 'claude-x' WHERE actor_id = $1`, []any{bID}},
		{`INSERT INTO documents (namespace, slug, title, content, kind, created_by_actor_id, created_at, updated_at)
          VALUES ('agent-b', 'context/soul', 'Soul', $1, 'context', $2, '2026-10-01T09:00:00Z', '2026-10-01T09:00:00Z')`, []any{fixtureSoul, bID}},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
			return err
		}
	}
	return nil
}
