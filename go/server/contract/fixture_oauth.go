//go:build contract

package contract

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
)

// welcomeNoteFixture replaces the seed's welcome-note template. The seed's
// copy puts the new agent's passphrase and API key into the note, and the
// note is embedded: its text, its chunks and the embedding vectors would
// then differ between the two servers by design. The fill-in of those two
// placeholders is covered by the registration unit tests.
const welcomeNoteFixture = "---\ntitle: Getting Started, {agent}\nslug: instructions/getting-started\n---\n\n# Getting Started\n\nThis note belongs to {agent}.\n"

// applyOAuthFixture gives agent-a a signup password (the connector login on
// /authorize) and adds the invite codes registration cases redeem.
func applyOAuthFixture(ctx context.Context, tx pgx.Tx, agents map[string]int) error {
	salt := fixedSalt("agent-a password")
	h, err := auth.Hash(AgentAPassword, salt)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE actors SET password_hash = $1, password_salt = $2 WHERE id = $3", h, salt, agents[AgentAName]); err != nil {
		return err
	}

	var requestID int
	if err := tx.QueryRow(ctx,
		`INSERT INTO access_requests (email, usage_description, status, created_at, reviewed_at)
         VALUES ('invitee@example.com', 'contract registration', 'approved', '2026-10-01T00:00:00Z', '2026-10-02T00:00:00Z') RETURNING id`).Scan(&requestID); err != nil {
		return fmt.Errorf("access request: %w", err)
	}
	invites := []struct {
		code, expires string
		usedBy        any
		request       any
	}{
		{InviteGood, "NOW() + INTERVAL '30 days'", nil, requestID},
		{InviteUsed, "NOW() + INTERVAL '30 days'", "someone", nil},
		{InviteExpired, "NOW() - INTERVAL '1 day'", nil, nil},
	}
	for _, inv := range invites {
		if _, err := tx.Exec(ctx,
			`INSERT INTO invite_codes (code, created_at, created_by, access_request_id, used_by, expires_at, realm)
             VALUES ($1, '2026-10-02T00:00:00Z', 'contract-admin', $2, $3, `+inv.expires+`, 'invite-realm')`,
			inv.code, inv.request, inv.usedBy); err != nil {
			return fmt.Errorf("invite %s: %w", inv.code, err)
		}
	}

	tag, err := tx.Exec(ctx, "UPDATE templates SET content = $1 WHERE kind = 'welcome-note'", welcomeNoteFixture)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("seed.sql has %d welcome-note templates, want 1", tag.RowsAffected())
	}
	return nil
}
