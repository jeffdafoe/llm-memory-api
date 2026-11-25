package enrichment

import (
	"context"
	"testing"
	"time"
)

// A sim-shared villager's sim-day log is a conversation, so it is skipped
// before the agent lookup; a note in the same villager folder is still
// enriched (LLM-1032).
func TestEnrichSkipsNestedConversationLogs(t *testing.T) {
	provider, model, key := "openrouter", "m", "k"
	cases := []struct {
		slug     string
		enriched bool
	}{
		{"lewis-walker/conversations/2026-11-24-sim-day", false},
		{"conversations/2026-11-24-abc", false},
		{"lewis-walker/memory/the-mill", true},
		{"projects/acme/conversations/kickoff", true},
	}
	for _, c := range cases {
		t.Run(c.slug, func(t *testing.T) {
			sc := &goldenScenario{
				Agent: &goldenAgent{Provider: &provider, Model: &model, APIKey: &key},
				Text:  `"episodic"`,
			}
			r := &recorder{sc: sc}
			e := &Enricher{
				DB: r, Agents: r, Log: r,
				Config: fakeConfig{map[string]string{"note_enrichment_enabled": "true"}},
				Now:    func() time.Time { return goldenNow },
			}
			if err := e.Enrich(context.Background(), "salem-vendor", c.slug, "t", "content", nil); err != nil {
				t.Fatal(err)
			}
			invoked := false
			for _, ev := range r.events {
				if ev.Kind == "invoke" {
					invoked = true
				}
			}
			if invoked != c.enriched {
				t.Errorf("model called = %v, want %v (events %+v)", invoked, c.enriched, r.events)
			}
		})
	}
}
