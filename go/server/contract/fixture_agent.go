//go:build contract

package contract

import (
	"context"
	"fmt"
	"strings"

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
	return applyMemorySyncFixture(ctx, tx, aID)
}

// syncNote is one fixture row for /agent/memory/sync. No chunks, so search
// never sees it; the texts match no grep case.
type syncNote struct {
	slug, content, updated, deleted, metadata string
}

// applyMemorySyncFixture adds agent-a's memory notes and conversation logs:
//   - instructions/memory/: a note newer than the cases' local copy, one at
//     the same millisecond (its microseconds differ), one older, one with no
//     extension (retired.md), "dup" and "dup.md" (both map to dup.md; the
//     newer is listed first and wins), a nested slug and a dot name (both
//     skipped), an upper-case slug (listed, but not under the prefix), a
//     ".txt" name, a soft-deleted note, and one updated in 2099 (after any
//     scan, so prune mode pulls it);
//   - conversations: a live log and its retired copy (sizes 100 and 300, so
//     the largest is 300), a soft-deleted log, and one whose stored size is
//     not a number.
func applyMemorySyncFixture(ctx context.Context, tx pgx.Tx, aID int) error {
	rows := []syncNote{
		{slug: "instructions/memory/remote-newer.md", content: "# Remote Newer\n\nKept on the server.", updated: "2026-10-02T10:00:00Z"},
		{slug: "instructions/memory/same.md", content: "Same on both sides.", updated: "2026-10-02T10:00:00.000500Z"},
		{slug: "instructions/memory/local-newer.md", content: "Older server copy.", updated: "2026-10-02T10:00:00Z"},
		{slug: "instructions/memory/retired", content: "Retired locally.", updated: "2026-10-01T09:00:00Z"},
		{slug: "instructions/memory/dup", content: "Dup without extension, newer.", updated: "2026-10-04T09:00:00Z"},
		{slug: "instructions/memory/dup.md", content: "Dup with extension, older.", updated: "2026-10-03T09:00:00Z"},
		{slug: "instructions/memory/nested/deep.md", content: "Nested.", updated: "2026-10-01T09:00:00Z"},
		{slug: "instructions/memory/.hidden", content: "Hidden.", updated: "2026-10-01T09:00:00Z"},
		{slug: "Instructions/Memory/Upper.md", content: "Upper case folder.", updated: "2026-10-01T09:00:00Z"},
		{slug: "instructions/memory/notes.txt", content: "Plain text file.", updated: "2026-10-01T08:00:00Z"},
		{slug: "instructions/memory/deleted.md", content: "Deleted on the server.", updated: "2026-10-01T09:00:00Z", deleted: "2026-10-01T10:00:00Z"},
		{slug: "instructions/memory/future.md", content: "Written after the scan.", updated: "2099-01-01T00:00:00Z"},

		{slug: "conversations/2026-10-01-" + syncSessionLive, content: "[10:00] agent-a: hello",
			updated:  "2026-10-01T09:00:00Z",
			metadata: `{"session_id": "` + syncSessionLive + `", "file_size": 100}`},
		{slug: "conversations/2026-09-01-" + syncSessionLive, content: "",
			updated: "2026-09-01T09:00:00Z", deleted: "2026-09-02T09:00:00Z",
			metadata: `{"session_id": "` + syncSessionLive + `", "file_size": "300", "purged_at": "2026-09-03T00:00:00.000Z"}`},
		{slug: "conversations/2026-09-05-" + syncSessionDeleted, content: "[09:00] agent-a: retired",
			updated: "2026-09-05T09:00:00Z", deleted: "2026-09-06T09:00:00Z",
			metadata: `{"session_id": "` + syncSessionDeleted + `", "file_size": 500}`},
		{slug: "conversations/2026-09-07-" + syncSessionOdd, content: "[08:00] agent-a: odd size",
			updated:  "2026-09-07T09:00:00Z",
			metadata: `{"session_id": "` + syncSessionOdd + `", "file_size": "12 bytes"}`},
	}
	for _, n := range rows {
		var deleted, meta any
		if n.deleted != "" {
			deleted = n.deleted
		}
		if n.metadata != "" {
			meta = n.metadata
		}
		kind := "note"
		title := strings.TrimPrefix(n.slug, "instructions/memory/")
		if strings.HasPrefix(n.slug, "conversations/") {
			kind, title = "conversation", "Conversation "+n.slug[len("conversations/"):]
		} else if strings.HasPrefix(strings.ToLower(n.slug), "instructions/") {
			kind = "instruction"
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO documents (namespace, slug, title, content, kind, metadata, created_by_actor_id, created_at, updated_at, deleted_at)
             VALUES ('agent-a', $1, $2, $3, $4, $5::jsonb, $6, '2026-09-01T00:00:00Z', $7, $8)`,
			n.slug, title, n.content, kind, meta, aID, n.updated, deleted); err != nil {
			return fmt.Errorf("memory-sync note %s: %w", n.slug, err)
		}
	}
	return nil
}
