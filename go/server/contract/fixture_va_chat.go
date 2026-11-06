//go:build contract

package contract

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// fixtureImpressions is va-openrouter's note on Anne Hale, whom the sim
// cases' perception names as co-located.
const fixtureImpressions = "Anne pays late but always pays."

// applyVAChatFixture adds what the direct-chat cases (cases_va_chat.go)
// need on top of the virtual agents of fixture_va.go: the salem-engine actor
// (no agent_configuration row, so it is not virtual and gets no "*"
// broadcast) with access to va-openrouter, the NPC provider, and
// va-openrouter's context/people/anne-hale note (no chunks, so search never
// sees it).
func applyVAChatFixture(ctx context.Context, tx pgx.Tx, adminID int, agents map[string]int) error {
	var engineID, vaID int
	if err := tx.QueryRow(ctx, "INSERT INTO actors (name, created_by) VALUES ($1, $2) RETURNING id", engineName, adminID).Scan(&engineID); err != nil {
		return fmt.Errorf("engine actor: %w", err)
	}
	if err := tx.QueryRow(ctx, "SELECT id FROM actors WHERE name = 'va-openrouter'").Scan(&vaID); err != nil {
		return fmt.Errorf("va-openrouter: %w", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO virtual_agent_access (virtual_agent_id, grantee_actor_id) VALUES ($1, $2)", vaID, engineID); err != nil {
		return fmt.Errorf("engine access: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO documents (namespace, slug, title, content, kind, created_by_actor_id, created_at, updated_at)
         VALUES ('va-openrouter', 'context/people/anne-hale', 'Anne Hale', $1, 'context', $2, '2026-10-01T09:00:00Z', '2026-10-01T09:00:00Z')`,
		fixtureImpressions, vaID); err != nil {
		return fmt.Errorf("impressions note: %w", err)
	}
	return nil
}
