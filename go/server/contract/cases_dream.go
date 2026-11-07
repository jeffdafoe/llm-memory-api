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

	// /admin/dream/consolidate-people. agent-b is the fixture's one agent
	// in a people-keeping dream mode (companion).
	{
		// Superadmin only (the limited admin holds agents:write), then the
		// body, the filters' types and form, and the named agent; with no
		// people writer the agent is a result entry.
		Name: "admin dream consolidate-people refusals",
		Steps: []Step{
			adminPost(consolidatePeoplePath, "admin-limited-web", `{}`),
			{Label: "no body", Method: "POST", Path: consolidatePeoplePath, Auth: "admin-web"},
			adminPost(consolidatePeoplePath, "admin-web", `{"agent":5}`),
			adminPost(consolidatePeoplePath, "admin-web", `{"agent":"agent-b","person_slug":["x"]}`),
			adminPost(consolidatePeoplePath, "admin-web", `{"person_slug":"Agent A"}`),
			adminPost(consolidatePeoplePath, "admin-web", `{"person_slug":"-agent-a"}`),
			adminPost(consolidatePeoplePath, "admin-web", `{"agent":"agent-a"}`),
			adminPost(consolidatePeoplePath, "admin-web", `{"agent":"nobody","dry_run":true}`),
			adminPost(consolidatePeoplePath, "admin-web", `{"agent":"","person_slug":null}`),
		},
	},
	{
		// Every relationship file of agent-b, newest first: a slug that is
		// not canonical is an error entry with no model call, an unchanged
		// reply is not proposed, a changed one is. Nothing is written.
		Name: "admin dream consolidate-people dry run",
		Steps: []Step{
			{SQL: consolidatePeopleSetup},
			adminPost(consolidatePeoplePath, "admin-web", `{"dry_run":true}`),
		},
		Replies: []QueuedReply{
			anthropicText("- quiet"),
			anthropicText("- likes tea\n- prefers Monday releases"),
			anthropicText("- owes nothing\n- paid for the lamp"),
		},
		Known: []KnownDifference{callDuration},
	},
	{
		// The agent and person filters pick one file; the changed reply is
		// saved over it (and indexed in the background).
		Name: "admin dream consolidate-people writes",
		Steps: []Step{
			{SQL: consolidatePeopleSetup},
			adminPost(consolidatePeoplePath, "admin-web", `{"agent":"agent-b","person_slug":"agent-a","dry_run":0}`),
			{Settle: true},
		},
		Replies: []QueuedReply{
			anthropicText("- likes tea\n- prefers Monday releases"),
		},
		OutboundMultiset: true,
		Known:            []KnownDifference{callDuration},
	},
	{
		// An empty reply and a cut-off one both keep the file.
		Name: "admin dream consolidate-people empty and truncated",
		Steps: []Step{
			{SQL: consolidatePeopleSetup},
			adminPost(consolidatePeoplePath, "admin-web", `{"person_slug":"dave"}`),
			adminPost(consolidatePeoplePath, "admin-web", `{"person_slug":"carol"}`),
		},
		Replies: []QueuedReply{
			anthropicText(""),
			anthropicStopped("- quiet but", "max_tokens"),
		},
		Known: []KnownDifference{callDuration},
	},
}

const consolidatePeoplePath = "/v1/admin/dream/consolidate-people"

// consolidatePeopleSetup makes va-anthropic (owned by the system actor, so
// the trust check passes) the companion people writer and gives agent-b
// four relationship files a day apart, and one note outside the folder.
const consolidatePeopleSetup = `
UPDATE actors SET expertise = '["dream-companion-people"]',
    created_by = (SELECT id FROM actors WHERE name = 'system')
    WHERE name = 'va-anthropic';
UPDATE agent_configuration SET storage_quota = NULL
    WHERE actor_id = (SELECT id FROM actors WHERE name = 'agent-b');
INSERT INTO documents (namespace, slug, title, content, created_at, updated_at) VALUES
    ('agent-b', 'context/people/Bad Slug', 'People — Bad', '- x', NOW() - INTERVAL '1 day', NOW() - INTERVAL '1 day'),
    ('agent-b', 'context/people/carol', NULL, '- quiet', NOW() - INTERVAL '2 days', NOW() - INTERVAL '2 days'),
    ('agent-b', 'context/people/agent-a', E'People — Agent\nA', E'- likes tea\n- likes tea\n- prefers Monday releases', NOW() - INTERVAL '3 days', NOW() - INTERVAL '3 days'),
    ('agent-b', 'context/people/dave', 'People — Dave ✓', '- owes nothing 🕯', NOW() - INTERVAL '4 days', NOW() - INTERVAL '4 days'),
    ('agent-b', 'notes/people', 'Not a person', '- y', NOW(), NOW());
`

// dreamSetup is the same text on both copies. The window starts ten
// minutes back and the log is five minutes old, far from either edge.
const dreamSetup = `
UPDATE config SET value = '0 4 * * *' WHERE key = 'dream_cron_schedule';
UPDATE config SET value = 'true' WHERE key = 'dream_processing_enabled';
UPDATE config SET value = '0' WHERE key IN ('dream_interagent_delay', 'dream_interchunk_delay');
UPDATE actors SET expertise = '["dream-companion", "dream-companion-soul", "dream-companion-people"]',
    created_by = (SELECT id FROM actors WHERE name = 'system')
    WHERE name = 'va-anthropic';
-- agent-b's 100-byte fixture quota would refuse whichever save lands after
-- the background usage counter catches up, a race on both servers.
UPDATE agent_configuration SET last_dream_at = NOW() - INTERVAL '10 minutes', storage_quota = NULL
    WHERE actor_id = (SELECT id FROM actors WHERE name = 'agent-b');
INSERT INTO documents (namespace, slug, title, content, kind, created_at, updated_at) VALUES
    ('agent-b', 'conversations/release-day', 'Release day',
     E'[10:00 agent-b] I want the release out by friday, remember that.\n[10:01 agent-a] Actually I prefer Monday, because the build is red.\n[10:02 agent-b] ok',
     'conversation', NOW() - INTERVAL '5 minutes', NOW() - INTERVAL '5 minutes');
`

// anthropicText is a Messages API answer with the given text.
func anthropicText(text string) QueuedReply {
	return anthropicStopped(text, "end_turn")
}

// anthropicStopped is a Messages API answer with the given text and stop
// reason ("max_tokens" is a cut-off reply).
func anthropicStopped(text, stopReason string) QueuedReply {
	body, err := json.Marshal(struct {
		ID         string     `json:"id"`
		Type       string     `json:"type"`
		Role       string     `json:"role"`
		Model      string     `json:"model"`
		Content    []textPart `json:"content"`
		StopReason string     `json:"stop_reason"`
		Usage      tokenUsage `json:"usage"`
	}{"msg_fake", "message", "assistant", fakeModel, []textPart{{"text", text}}, stopReason, tokenUsage{10, 5}})
	if err != nil {
		panic(err)
	}
	return QueuedReply{Upstream: "anthropic", Path: "/v1/messages", Reply: Reply{Status: 200, Body: body}}
}
