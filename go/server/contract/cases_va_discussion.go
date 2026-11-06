package contract

import (
	"context"
	"fmt"
	"strconv"
)

// The discussions whose virtual participants answer (fixture_va_discussion.go).
const (
	panelDiscussion   = 110
	keylessDiscussion = 111
	// failingDiscussion has one virtual participant: the [Malfunction]
	// post triggers the discussion again, and with a second one the leave
	// would race that trigger's participant read.
	failingDiscussion = 112
	panelVote         = 206
)

// panelAgents are the panel discussion's virtual participants: Anthropic
// first, then OpenAI, the order the trigger serves them in.
var panelAgents = []fixtureVirtualAgent{
	{name: "panel-ann", provider: "anthropic", model: "claude-haiku-4-5", configVersion: 1},
	{name: "panel-bob", provider: "openai", model: "gpt-6-luna", configVersion: 3},
}

// discussionSay is vaUser's message to a discussion, sent by the admin on
// its behalf; the chat send starts the virtual-agent trigger.
func discussionSay(discussionID int, message string) Step {
	return Step{Method: "POST", Path: chatSendPath, Auth: "admin-web", Header: jsonType,
		Body: fmt.Sprintf(`{"from_agent":%q,"discussion_id":%d,"message":%q}`, vaUser, discussionID, message)}
}

// panelPosted waits until each named agent has posted n messages to the
// discussion (error rows when isError) and every panel agent's activity
// spinner is off (it goes off 3 s after the call, past a quiet settle).
// Only rows the case wrote count: the fixture's are below id 100.
func panelPosted(discussionID int, isError bool, n int, names ...string) Step {
	return Step{Settle: true, Until: func(ctx context.Context, t *Target) (bool, error) {
		for _, name := range names {
			ok, err := t.Holds(ctx, `SELECT count(*) >= $4 FROM chat_message_texts cmt
                JOIN actors f ON f.id = cmt.from_actor_id
                WHERE f.name = $1 AND cmt.discussion_id = $2 AND cmt.is_error = $3 AND cmt.id > 100`, name, discussionID, isError, n)
			if err != nil || !ok {
				return ok, err
			}
		}
		return t.Holds(ctx, `SELECT NOT EXISTS (SELECT 1 FROM actors WHERE name IN ($1, $2) AND active_since IS NOT NULL)`,
			panelAgents[0].name, panelAgents[1].name)
	}}
}

// panelFailed waits until the agent has left the discussion in error
// status and its spinner is off.
func panelFailed(name string, discussionID int) Step {
	return Step{Settle: true, Until: func(ctx context.Context, t *Target) (bool, error) {
		return t.Holds(ctx, `SELECT EXISTS (SELECT 1 FROM discussion_participants dp JOIN actors a ON a.id = dp.actor_id
                WHERE a.name = $1 AND dp.discussion_id = $2 AND dp.status = 'left')
            AND EXISTS (SELECT 1 FROM actors WHERE name = $1 AND status = 'error' AND active_since IS NULL)`, name, discussionID)
	}}
}

// ballotsCast waits until n ballots are in the vote and the spinners are off.
func ballotsCast(voteID, n int) Step {
	return Step{Settle: true, Until: func(ctx context.Context, t *Target) (bool, error) {
		return t.Holds(ctx, `SELECT (SELECT count(*) FROM discussion_ballots WHERE vote_id = $1) >= $2
            AND NOT EXISTS (SELECT 1 FROM actors WHERE name IN ($3, $4) AND active_since IS NOT NULL)`,
			voteID, n, panelAgents[0].name, panelAgents[1].name)
	}}
}

func voteText(choice int, reason string) string {
	return strconv.Quote(fmt.Sprintf(`{"choice": %d, "reason": %q}`, choice, reason))
}

// panelVoteReplies are each panel agent's ballot: Ann approves, Bob refuses.
var panelVoteReplies = []QueuedReply{
	{Upstream: "anthropic", Path: "/v1/messages", Reply: JSONReply(200,
		`{"id":"msg_vote","type":"message","role":"assistant","model":"fake-model","content":[{"type":"text","text":`+voteText(1, "Pies sell well.")+`}],`+
			`"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)},
	{Upstream: "openai", Path: "/v1/responses", Reply: JSONReply(200,
		`{"id":"resp_vote","object":"response","status":"completed","model":"fake-model","output":[{"type":"message","role":"assistant",`+
			`"content":[{"type":"output_text","text":`+voteText(2, "Bread keeps longer.")+`}]}],"usage":{"input_tokens":10,"output_tokens":5}}`)},
}

// vaDiscussionCases have a discussion's virtual participants answer a
// message (everyone, or the one it names), cast a vote, refuse for want of
// a key, and fail at the provider. Each reply posts to the discussion and
// so triggers again, which the loop check stops. The fixture's response
// delay and stagger are 1 s each.
var vaDiscussionCases = []Scenario{
	{
		Name:  "discussion message to its virtual participants",
		Steps: []Step{discussionSay(panelDiscussion, "Good evening, friends. What shall we bring to the fair?"), panelPosted(panelDiscussion, false, 1, panelAgents[0].name, panelAgents[1].name)},
		Known: []KnownDifference{callDuration},
	},
	{
		Name:  "discussion message addressed to one virtual participant",
		Steps: []Step{discussionSay(panelDiscussion, "Ann, will you bake the pies?"), panelPosted(panelDiscussion, false, 1, panelAgents[0].name)},
		Known: []KnownDifference{callDuration},
	},
	{
		// The second message replays the first exchange as history.
		Name: "discussion message twice",
		Steps: []Step{
			discussionSay(panelDiscussion, "Ann, will you bake the pies?"), panelPosted(panelDiscussion, false, 1, panelAgents[0].name),
			discussionSay(panelDiscussion, "Bob, will you bring the bread?"), panelPosted(panelDiscussion, false, 1, panelAgents[1].name),
		},
		Known: []KnownDifference{callDuration},
	},
	{
		// A vote trigger as the vote/propose service sends it: each agent's
		// ballot, the vote evaluated (two of three in, so it stays open),
		// and each reason posted.
		Name:    "discussion vote by its virtual participants",
		Replies: panelVoteReplies,
		Steps: []Step{
			{Method: "POST", Path: chatSendPath, Auth: "agent-a-session", Header: jsonType,
				Body: fmt.Sprintf(`{"to_agents":["system"],"message":"{\"type\":\"virtual-agent\",\"discussionId\":%d,\"triggerType\":\"vote-proposed\",\"voteId\":%d}"}`, panelDiscussion, panelVote)},
			ballotsCast(panelVote, 2),
		},
		Known: []KnownDifference{callDuration},
	},
	{
		Name:  "discussion virtual participant with no API key",
		Steps: []Step{discussionSay(keylessDiscussion, "Anyone there?"), panelPosted(keylessDiscussion, true, 1, "va-no-key")},
	},
	{
		// One attempt: the error usage and call rows, the [Malfunction]
		// post, the leave and its event, and the agent's error status.
		Name:    "discussion virtual participant whose provider fails",
		Replies: []QueuedReply{anthropicError},
		Steps:   []Step{discussionSay(failingDiscussion, "Good evening."), panelFailed(panelAgents[0].name, failingDiscussion)},
	},
}
