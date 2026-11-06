//go:build contract

package contract

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// applyVADiscussionFixture adds the discussions whose virtual participants
// answer (cases_va_discussion.go): panelDiscussion, active, with vaUser and
// the two panel agents joined and an open vote, keylessDiscussion with
// vaUser and va-no-key joined, and failingDiscussion with vaUser and the
// first panel agent. The panel agents are their own virtual agents
// (one on Anthropic, one on OpenAI) with learning off: a discussion prompt
// is long enough for learning extraction, whose note slug is the second it
// ran. Their dream mode is 'none', so no transcript is written either.
func applyVADiscussionFixture(ctx context.Context, tx pgx.Tx, adminID int) error {
	ids := map[string]int{}
	for _, name := range []string{vaUser, "va-no-key"} {
		var id int
		if err := tx.QueryRow(ctx, "SELECT id FROM actors WHERE name = $1", name).Scan(&id); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		ids[name] = id
	}
	for _, va := range panelAgents {
		var id int
		if err := tx.QueryRow(ctx, "INSERT INTO actors (name, created_by) VALUES ($1, $2) RETURNING id", va.name, adminID).Scan(&id); err != nil {
			return fmt.Errorf("panel agent %s: %w", va.name, err)
		}
		key, err := encryptFixtureKey(va.name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO agent_configuration (actor_id, virtual, provider, model, api_key, configuration, startup_instructions, learning_enabled)
             VALUES ($1, true, $2, $3, $4, $5, $6, false)`,
			id, va.provider, va.model, key, fmt.Sprintf(`{"_configVersion":%d}`, va.configVersion),
			"You are "+va.name+", a contract fixture on a panel."); err != nil {
			return fmt.Errorf("panel agent %s configuration: %w", va.name, err)
		}
		ids[va.name] = id
	}

	for _, d := range []struct {
		id      int
		topic   string
		context *string
		members []string
	}{
		{panelDiscussion, "Plan the harvest fair", ptr("Last year the pies sold out by noon."), []string{vaUser, panelAgents[0].name, panelAgents[1].name}},
		{keylessDiscussion, "A keyless panel", nil, []string{vaUser, "va-no-key"}},
		{failingDiscussion, "A failing panel", nil, []string{vaUser, panelAgents[0].name}},
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO discussions (id, topic, status, mode, context, created_by_actor_id, created_at)
             VALUES ($1, $2, 'active', 'realtime', $3, $4, '2026-10-10 09:00:00+00')`, d.id, d.topic, d.context, ids[vaUser]); err != nil {
			return fmt.Errorf("discussion %d: %w", d.id, err)
		}
		for _, m := range d.members {
			if _, err := tx.Exec(ctx,
				`INSERT INTO discussion_participants (discussion_id, actor_id, status, role, invited_at, joined_at)
                 VALUES ($1, $2, 'joined', 'required', '2026-10-10 09:00:00+00', '2026-10-10 09:00:00+00')`, d.id, ids[m]); err != nil {
				return fmt.Errorf("participant %s of %d: %w", m, d.id, err)
			}
		}
	}
	// closes_at is set, so the stale-vote scanner never takes it.
	if _, err := tx.Exec(ctx,
		`INSERT INTO discussion_votes (id, discussion_id, question, status, type, threshold, closes_at, created_at, proposed_by_actor_id)
         VALUES ($1, $2, 'Bring pies to the fair? 1=yes 2=no', 'open', 'general', 'unanimous', '2099-01-01 00:00:00+00', '2026-10-10 09:30:00+00', $3)`,
		panelVote, panelDiscussion, ids[vaUser]); err != nil {
		return fmt.Errorf("panel vote: %w", err)
	}
	return nil
}
