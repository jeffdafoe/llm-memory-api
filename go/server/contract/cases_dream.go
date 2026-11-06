package contract

import "encoding/json"

// dreamCases run one dream (the scheduler's callback) on each server's
// database copy. The setup turns dream processing on, makes va-anthropic
// (owned by the system actor, so the trust check passes) the companion
// dream, soul and people agent, and gives agent-b (dream mode companion in
// the fixture) a conversation log inside its window. The run writes the
// dream note, rebuilds agent-b's short fixture soul from its dreams, and
// writes agent-a's relationship file; the rows, the notes' chunks and the
// provider requests are compared, and the job's [dream] lines are the
// step's body.
var dreamCases = []Scenario{
	{
		Name: "dream run",
		Steps: []Step{
			{SQL: dreamSetup},
			{Job: "dream"},
		},
		Replies: []QueuedReply{
			anthropicText("# Release Planning\n\nagent-b and agent-a planned the release for Friday."),
			anthropicText("Agent B keeps a steady watch over releases and remembers what was promised."),
			anthropicText("- prefers Monday releases"),
		},
		// The saved notes are indexed in the background, so their
		// embedding calls interleave with the model calls.
		OutboundMultiset: true,
		Known:            []KnownDifference{callDuration},
	},
	{
		// A second run in the same window finds the cursor advanced and the
		// log already consumed: one chunk with no logs, no model call.
		Name: "dream run twice",
		Steps: []Step{
			{SQL: dreamSetup},
			{Job: "dream"},
			{Job: "dream"},
		},
		Replies: []QueuedReply{
			anthropicText("# Release Planning"),
			anthropicText("Agent B keeps a steady watch."),
			anthropicText("- prefers Monday releases"),
		},
		OutboundMultiset: true,
		Known:            []KnownDifference{callDuration},
	},
	{
		// Processing off: the run logs its skip and touches nothing.
		Name:  "dream run disabled",
		Steps: []Step{{SQL: `UPDATE config SET value = '0 4 * * *' WHERE key = 'dream_cron_schedule';`}, {Job: "dream"}},
	},
}

// dreamSetup is the same text on both copies. The window starts ten
// minutes back and the log is five minutes old, far from either edge.
const dreamSetup = `
UPDATE config SET value = '0 4 * * *' WHERE key = 'dream_cron_schedule';
UPDATE config SET value = 'true' WHERE key = 'dream_processing_enabled';
UPDATE config SET value = '0' WHERE key IN ('dream_interagent_delay', 'dream_interchunk_delay');
UPDATE actors SET expertise = '["dream-companion", "dream-companion-soul", "dream-companion-people"]',
    created_by = (SELECT id FROM actors WHERE name = 'system')
    WHERE name = 'va-anthropic';
UPDATE agent_configuration SET last_dream_at = NOW() - INTERVAL '10 minutes'
    WHERE actor_id = (SELECT id FROM actors WHERE name = 'agent-b');
INSERT INTO documents (namespace, slug, title, content, kind, created_at, updated_at) VALUES
    ('agent-b', 'conversations/release-day', 'Release day',
     E'[10:00 agent-b] I want the release out by friday, remember that.\n[10:01 agent-a] Actually I prefer Monday, because the build is red.\n[10:02 agent-b] ok',
     'conversation', NOW() - INTERVAL '5 minutes', NOW() - INTERVAL '5 minutes');
`

// anthropicText is a Messages API answer with the given text.
func anthropicText(text string) QueuedReply {
	body, err := json.Marshal(struct {
		ID         string     `json:"id"`
		Type       string     `json:"type"`
		Role       string     `json:"role"`
		Model      string     `json:"model"`
		Content    []textPart `json:"content"`
		StopReason string     `json:"stop_reason"`
		Usage      tokenUsage `json:"usage"`
	}{"msg_fake", "message", "assistant", fakeModel, []textPart{{"text", text}}, "end_turn", tokenUsage{10, 5}})
	if err != nil {
		panic(err)
	}
	return QueuedReply{Upstream: "anthropic", Path: "/v1/messages", Reply: Reply{Status: 200, Body: body}}
}
