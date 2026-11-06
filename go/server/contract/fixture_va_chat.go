//go:build contract

package contract

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
)

// fixtureImpressions is va-openrouter's note on Anne Hale, whom the sim
// cases' perception names as co-located.
const fixtureImpressions = "Anne pays late but always pays."

// applyVAChatFixture adds what the direct-chat cases (cases_va_chat.go)
// need on top of the virtual agents of fixture_va.go: the salem-engine actor
// (no agent_configuration row, so it is not virtual and gets no "*"
// broadcast) with access to va-openrouter, the NPC provider;
// va-openrouter's context/people/anne-hale note (no chunks, so search never
// sees it); and history between the two in three scopes (fixtureHistory).
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
	// Its API key, as the engine authenticates for the /v1/sim routes.
	plain := vaKey(engineName)
	salt := fixedSalt(engineName + " api key")
	keyHash, err := auth.Hash(plain, salt)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO agent_api_keys (actor_id, key_hash, key_salt, key_lookup_hash, label) VALUES ($1, $2, $3, $4, 'contract')",
		engineID, keyHash, salt, auth.TokenLookupHash(plain)); err != nil {
		return fmt.Errorf("engine api key: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO documents (namespace, slug, title, content, kind, created_by_actor_id, created_at, updated_at)
         VALUES ('va-openrouter', 'context/people/anne-hale', 'Anne Hale', $1, 'context', $2, '2026-10-01T09:00:00Z', '2026-10-01T09:00:00Z')`,
		fixtureImpressions, vaID); err != nil {
		return fmt.Errorf("impressions note: %w", err)
	}
	for _, h := range fixtureHistory {
		from, to := engineID, vaID
		if h.fromVA {
			from, to = vaID, engineID
		}
		var scene *string
		if h.scene != "" {
			scene = &h.scene
		}
		// Minutes before now, so the rows are inside the history window
		// (virtual_agent_chat_history_hours) whenever the run starts.
		if _, err := tx.Exec(ctx,
			`INSERT INTO chat_message_texts (id, message, from_actor_id, sent_at, scene_id, is_error)
             VALUES ($1, $2, $3, NOW() - make_interval(mins => $4), $5::uuid, $6)`,
			h.id, h.message, from, h.minutesAgo, scene, h.isError); err != nil {
			return fmt.Errorf("history text %d: %w", h.id, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO chat_messages (id, message_text_id, to_actor_id, acked_at) VALUES ($1, $1, $2, NOW())`, h.id, to); err != nil {
			return fmt.Errorf("history delivery %d: %w", h.id, err)
		}
	}
	return nil
}

// fixtureHistory is salem-engine and va-openrouter's chat: a pair exchange
// with no scene, one in scene A, one in scene B, and an error row. Ids are
// below 100, where applyChatFixture starts the sequences. Each perception is
// followed by a reply, so pruning keeps them all.
var fixtureHistory = []struct {
	id         int
	message    string
	fromVA     bool
	scene      string
	minutesAgo int
	isError    bool
}{
	{id: 51, message: "The pair's own perception.", minutesAgo: 50},
	{id: 52, message: "The pair's own reply.", fromVA: true, minutesAgo: 49},
	{id: 53, message: "Scene A perception.", scene: historySceneA, minutesAgo: 40},
	{id: 54, message: "Scene A reply.", fromVA: true, scene: historySceneA, minutesAgo: 39},
	{id: 55, message: "Scene B perception.", scene: historySceneB, minutesAgo: 30},
	{id: 56, message: "Scene B reply.", fromVA: true, scene: historySceneB, minutesAgo: 29},
	{id: 57, message: "[Error] va-openrouter is unavailable (boom).", fromVA: true, minutesAgo: 20, isError: true},
}
