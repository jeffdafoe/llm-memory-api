package contract

import (
	"context"
	"strings"
)

// discussionCases cover the /v1/discussion routes against the discussion
// fixture (fixture_discussion.go, and discussion 101 in fixture_chat.go).
// No case joins helper-va or makes a discussion it has joined active, so
// Node's virtual-agent trigger never reaches a provider.
var discussionCases = concat(discussionCreateCases, discussionListCases, discussionStatusCases, discussionPendingCases,
	discussionConcludeCases, discussionJoinCases, discussionDeferCases, discussionLeaveCases,
	discussionProposeCases, discussionCastCases, discussionVoteStatusCases, discussionRoutingCases)

// The discussion fixture, beside discussion 101 (fixture_chat.go). helper-va
// is never joined in an active discussion, so Node's virtual-agent trigger
// finds no one to run. Every open vote has a closes_at, so the stale-vote
// scanner (votes with none, older than 30 minutes) never picks one up.
const (
	// waitingDiscussion: agent-a created and joined it; agent-b is invited.
	waitingDiscussion = "102"
	// pastTimeoutOptional: agent-a and agent-b joined, helper-va optional
	// and invited, timeout passed. Evaluating it times helper-va out and
	// makes it active.
	pastTimeoutOptional = "103"
	// concludedDiscussion: concluded, everyone left.
	concludedDiscussion = "104"
	// asyncDiscussion: active async, agent-a and agent-b joined, with votes
	// 201 to 204.
	asyncDiscussion = "105"
	// pastTimeoutRequired: agent-a created and joined it, agent-b invited
	// (and at the defer limit, 3), timeout passed. Evaluating it times it
	// out (abandoned). It is the highest id either agent is in, so it is
	// what create's conflict check looks at first.
	pastTimeoutRequired = "109"

	// Votes of the async discussion. 201: general unanimous, agent-a voted
	// 1. 202: conclude majority, no ballots. 203: general unanimous,
	// closed, agent-a 1 and agent-b 2. 204: general majority, closes_at
	// passed, no ballots.
	unanimousVote = "201"
	concludeVote  = "202"
	closedVote    = "203"
	expiredVote   = "204"
	// realtimeVote is an open vote in discussion 101 (realtime).
	realtimeVote = "205"
)

const (
	discussionCreatePath     = "/v1/discussion/create"
	discussionListPath       = "/v1/discussion/list"
	discussionStatusPath     = "/v1/discussion/status"
	discussionPendingPath    = "/v1/discussion/pending"
	discussionConcludePath   = "/v1/discussion/conclude"
	discussionCancelPath     = "/v1/discussion/cancel"
	discussionJoinPath       = "/v1/discussion/join"
	discussionDeferPath      = "/v1/discussion/defer"
	discussionLeavePath      = "/v1/discussion/leave"
	discussionProposePath    = "/v1/discussion/vote/propose"
	discussionCastPath       = "/v1/discussion/vote/cast"
	discussionVoteStatusPath = "/v1/discussion/vote/status"
)

func discussionPost(path, auth, body string) Step {
	return Step{Method: "POST", Path: path, Auth: auth, Header: jsonType, Body: body}
}

var settleStep = Step{Settle: true}

// notAStringWithDiscussionRows is notAStringInAdminSession for a request
// Node completed: it also wrote the rows Go never writes.
func notAStringWithDiscussionRows(label string, tables ...string) []KnownDifference {
	known := notAStringInAdminSession(label)
	for _, t := range tables {
		known = append(known, KnownDifference{Where: "table " + t, Reason: "Node stored the value Go refused (go-server note, Known differences)"})
	}
	return known
}

var discussionCreateCases = []Scenario{
	postCase("discussion create as an admin", discussionCreatePath, "admin-web",
		`{"topic":"A new topic","created_by":"contract-admin","participants":["contract-admin","agent-a"],"optional_participants":["agent-b"],"channel":" room-1! ","mode":"async","context":"Some context."}`),
	postCase("discussion create clears a timed-out conflict", discussionCreatePath, "agent-a-session",
		`{"topic":" Spaced topic ","participants":["agent-a"," Agent-B "],"mode":"nonsense","context":""}`),
	postCase("discussion create long context", discussionCreatePath, "admin-web",
		`{"topic":"Long","created_by":"contract-admin","participants":["contract-admin","agent-a"],"context":"`+strings.Repeat("c", 10050)+`"}`),
	{
		Name: "discussion create twice",
		Steps: []Step{
			discussionPost(discussionCreatePath, "agent-b-session", `{"topic":"First","participants":["agent-b","agent-a"]}`),
			settleStep,
			discussionPost(discussionCreatePath, "agent-b-session", `{"topic":"Second","participants":["agent-b","agent-a"]}`),
		},
	},
	{
		Name: "discussion create conflict with an active discussion",
		Steps: []Step{
			discussionPost("/v1/agent/heartbeat", "agent-a-session", `{}`),
			discussionPost(discussionStatusPath, "agent-a-session", `{"discussion_id":`+pastTimeoutRequired+`}`),
			discussionPost(discussionCreatePath, "agent-a-session", `{"topic":"Blocked","participants":["agent-a","agent-b"]}`),
		},
	},
	postCase("discussion create created_by mismatch", discussionCreatePath, "agent-a-session",
		`{"topic":"t","created_by":"agent-b","participants":["agent-a","agent-b"]}`),
	postCase("discussion create created_by sanitized to the agent", discussionCreatePath, "agent-a-session",
		`{"topic":"t","created_by":" Agent-A ","participants":["agent-a","agent-b"]}`),
	postCase("discussion create as an admin without created_by", discussionCreatePath, "admin-web",
		`{"topic":"t","participants":["agent-a","agent-b"]}`),
	postCase("discussion create without topic", discussionCreatePath, "agent-a-session", `{"participants":["agent-a","agent-b"]}`),
	postCase("discussion create topic sanitized to nothing", discussionCreatePath, "agent-a-session",
		`{"topic":"​","participants":["agent-a","agent-b"]}`),
	postCase("discussion create participants a string", discussionCreatePath, "agent-a-session",
		`{"topic":"t","participants":"agent-a,agent-b"}`),
	postCase("discussion create one participant", discussionCreatePath, "agent-a-session", `{"topic":"t","participants":["agent-a"]}`),
	postCase("discussion create one participant plus an optional one", discussionCreatePath, "admin-web",
		`{"topic":"t","created_by":"contract-admin","participants":["contract-admin"],"optional_participants":["agent-a"]}`),
	postCase("discussion create optional participants not an array", discussionCreatePath, "agent-a-session",
		`{"topic":"t","participants":["agent-a"],"optional_participants":"agent-b"}`),
	postCase("discussion create creator not a participant", discussionCreatePath, "agent-a-session",
		`{"topic":"t","participants":["agent-b"],"optional_participants":["agent-a"]}`),
	postCase("discussion create required and optional overlap", discussionCreatePath, "agent-a-session",
		`{"topic":"t","participants":["agent-a","agent-b"],"optional_participants":["agent-b","agent-b"]}`),
	postCase("discussion create an unknown participant", discussionCreatePath, "agent-a-session",
		`{"topic":"t","participants":["agent-a","nobody-here"]}`),
	postCase("discussion create an unknown optional participant", discussionCreatePath, "admin-web",
		`{"topic":"t","created_by":"contract-admin","participants":["contract-admin","agent-a"],"optional_participants":["nobody-here"]}`),
	postCase("discussion create an unknown creator", discussionCreatePath, "admin-web",
		`{"topic":"t","created_by":"nobody-here","participants":["nobody-here","agent-a"]}`),
	postCase("discussion create a repeated participant", discussionCreatePath, "admin-web",
		`{"topic":"t","created_by":"contract-admin","participants":["contract-admin","agent-a","agent-a"]}`),
	postCase("discussion create topic a number", discussionCreatePath, "admin-web",
		`{"topic":5,"created_by":"contract-admin","participants":["contract-admin","agent-a"]}`,
		notAStringWithDiscussionRows("POST "+discussionCreatePath, "discussion*", "chat_message*")...),
	postCase("discussion create a participant a number", discussionCreatePath, "admin-web",
		`{"topic":"t","created_by":"contract-admin","participants":["contract-admin",5]}`,
		notAStringInAdminSession("POST "+discussionCreatePath)...),
}

var discussionListCases = []Scenario{
	postCase("discussion list as an admin", discussionListPath, "admin-web", `{}`),
	postCase("discussion list as an admin by status", discussionListPath, "admin-web", `{"status":"active"}`),
	postCase("discussion list as an admin for an agent", discussionListPath, "admin-web", `{"agent":" Agent-B ","status":"waiting"}`),
	postCase("discussion list as an admin for an unknown agent", discussionListPath, "admin-web", `{"agent":"nobody-here"}`),
	postCase("discussion list as an agent ignores the agent field", discussionListPath, "agent-a-session", `{"agent":"agent-b"}`),
	postCase("discussion list as an agent by status", discussionListPath, "agent-b-key", `{"status":"concluded"}`),
	postCase("discussion list status a number", discussionListPath, "agent-a-session", `{"status":5}`),
	postCase("discussion list status an array", discussionListPath, "agent-a-session", `{"status":["active"]}`),
	postCase("discussion list as an admin agent a number", discussionListPath, "admin-web", `{"agent":5}`,
		notAStringInAdminSession("POST "+discussionListPath)...),
}

// statusAfterOwnHeartbeat reads a discussion's participants after the
// caller's own heartbeat. Node's participant query has no ORDER BY and reads
// actors in heap order; every /v1 request stamps the caller's last_seen in
// the background, which moves its actors row, so whether that write lands
// first decides the order. The synchronous stamp moves it first.
func statusAfterOwnHeartbeat(name, auth, body string) Scenario {
	return Scenario{Name: name, Steps: []Step{
		discussionPost("/v1/agent/heartbeat", auth, `{}`),
		discussionPost(discussionStatusPath, auth, body),
	}}
}

var discussionStatusCases = []Scenario{
	statusAfterOwnHeartbeat("discussion status", "agent-a-session", `{"discussion_id":101}`),
	statusAfterOwnHeartbeat("discussion status closes an expired vote", "agent-b-session", `{"discussion_id":`+asyncDiscussion+`}`),
	statusAfterOwnHeartbeat("discussion status times out a waiting discussion", "agent-a-session", `{"discussion_id":`+pastTimeoutRequired+`}`),
	postCase("discussion status activates without the optional member", discussionStatusPath, "admin-web", `{"discussion_id":`+pastTimeoutOptional+`}`),
	statusAfterOwnHeartbeat("discussion status of an invitee", "agent-b-session", `{"discussion_id":"`+waitingDiscussion+`"}`),
	postCase("discussion status concluded", discussionStatusPath, "admin-web", `{"discussion_id":`+concludedDiscussion+`}`),
	postCase("discussion status without an id", discussionStatusPath, "agent-a-session", `{}`),
	postCase("discussion status unknown", discussionStatusPath, "agent-a-session", `{"discussion_id":999}`),
	postCase("discussion status id not a number", discussionStatusPath, "agent-a-session", `{"discussion_id":"abc"}`),
	{
		Name: "discussion status of a non-participant",
		Steps: []Step{
			{Method: "POST", Path: discussionCreatePath, Auth: "admin-web", Header: jsonType,
				Body:    `{"topic":"Admin only","created_by":"contract-admin","participants":["contract-admin","agent-a"]}`,
				Capture: map[string]string{"id": "discussion.id"}},
			settleStep,
			discussionPost(discussionStatusPath, "agent-b-session", `{"discussion_id":{{id}}}`),
		},
	},
}

var discussionPendingCases = []Scenario{
	postCase("discussion pending", discussionPendingPath, "agent-b-session", `{}`),
	postCase("discussion pending of the other agent", discussionPendingPath, "agent-a-key", `{"agent":"agent-a"}`),
	postCase("discussion pending with a realtime discussion", discussionPendingPath, "agent-b-session", `{"discussion_id":101}`),
	postCase("discussion pending as an admin", discussionPendingPath, "admin-web", `{"agent":"agent-b","discussion_id":"`+asyncDiscussion+`"}`),
	postCase("discussion pending as an admin without agent", discussionPendingPath, "admin-web", `{}`),
	postCase("discussion pending as an admin for an unknown agent", discussionPendingPath, "admin-web", `{"agent":"nobody-here"}`),
	postCase("discussion pending agent mismatch", discussionPendingPath, "agent-b-session", `{"agent":"agent-a"}`),
	postCase("discussion pending id not a number", discussionPendingPath, "agent-b-session", `{"discussion_id":"abc"}`),
}

// deliveryRace is a conclude or cancel: Node started the participant
// messages and the discussion event without awaiting either, so which chain
// took the lower ids was a race between commits. The texts are ordered by
// content (pg.go tableOrder) and each delivery carries the content of the
// text it points at (linkChatDeliveries), so only the ids are let go and
// the link from delivery to text is still compared.
var deliveryRace = []KnownDifference{
	{Where: "table chat_message_texts row *.id", Volatile: true, Reason: raceReason},
	{Where: "table chat_messages row *.id", Volatile: true, Reason: raceReason},
	{Where: "table chat_messages row *.message_text_id", Volatile: true, Reason: raceReason},
}

const raceReason = "Node's conclude writes the event and the participant messages concurrently; their ids depend on which commit returns first"

// proposeRace is a vote propose: the discussion event and the virtual
// agents' vote notice are both started without awaiting either (Node, and
// Go's Service.Propose), so which text takes the lower id is a race.
var proposeRace = []KnownDifference{
	{Where: "table chat_message_texts row *.id", Volatile: true, Reason: proposeRaceReason},
	{Where: "table chat_messages row *.id", Volatile: true, Reason: proposeRaceReason},
	{Where: "table chat_messages row *.message_text_id", Volatile: true, Reason: proposeRaceReason},
}

const proposeRaceReason = "a vote propose writes the discussion event and the virtual agents' notice concurrently; their ids depend on which insert runs first"

var discussionConcludeCases = []Scenario{
	postCase("discussion conclude", discussionConcludePath, "agent-a-session", `{"discussion_id":`+asyncDiscussion+`}`, deliveryRace...),
	postCase("discussion conclude with an invited member", discussionConcludePath, "agent-b-session", `{"discussion_id":101}`, deliveryRace...),
	postCase("discussion conclude a waiting discussion as its creator", discussionConcludePath, "agent-a-session", `{"discussion_id":"`+waitingDiscussion+`"}`, deliveryRace...),
	postCase("discussion conclude a waiting discussion not as its creator", discussionConcludePath, "agent-b-session", `{"discussion_id":`+pastTimeoutOptional+`}`),
	postCase("discussion conclude not joined", discussionConcludePath, "agent-b-session", `{"discussion_id":`+waitingDiscussion+`}`),
	postCase("discussion conclude after leaving", discussionConcludePath, "agent-a-session", `{"discussion_id":`+concludedDiscussion+`}`),
	postCase("discussion conclude unknown", discussionConcludePath, "agent-a-session", `{"discussion_id":999}`),
	postCase("discussion conclude without an id", discussionConcludePath, "agent-a-session", `{}`),
	postCase("discussion conclude as an admin without agent", discussionConcludePath, "admin-web", `{"discussion_id":101}`),
	postCase("discussion conclude as an admin for an unknown agent", discussionConcludePath, "admin-web", `{"discussion_id":101,"agent":"nobody-here"}`),
	postCase("discussion conclude agent mismatch", discussionConcludePath, "agent-a-session", `{"discussion_id":101,"agent":"agent-b"}`),
	postCase("discussion conclude id not a number", discussionConcludePath, "agent-a-session", `{"discussion_id":"abc"}`),
	postCase("discussion cancel", discussionCancelPath, "agent-b-session", `{"discussion_id":`+asyncDiscussion+`}`, deliveryRace...),
	postCase("discussion cancel a waiting discussion", discussionCancelPath, "agent-a-session", `{"discussion_id":`+waitingDiscussion+`}`, deliveryRace...),
	postCase("discussion cancel without an id", discussionCancelPath, "agent-a-session", `{}`),
	postCase("discussion cancel as an admin without agent", discussionCancelPath, "admin-web", `{"discussion_id":101}`),
	postCase("discussion cancel not joined", discussionCancelPath, "agent-b-session", `{"discussion_id":`+pastTimeoutRequired+`}`),
}

var discussionJoinCases = []Scenario{
	postCase("discussion join makes it active", discussionJoinPath, "agent-b-session", `{"discussion_id":"`+waitingDiscussion+`"}`),
	postCase("discussion join a timed-out discussion", discussionJoinPath, "agent-b-session", `{"discussion_id":`+pastTimeoutRequired+`}`),
	postCase("discussion join already joined", discussionJoinPath, "agent-a-session", `{"discussion_id":101}`),
	postCase("discussion join a concluded discussion", discussionJoinPath, "agent-b-session", `{"discussion_id":`+concludedDiscussion+`}`),
	postCase("discussion join as a new participant", discussionJoinPath, "admin-web", `{"discussion_id":101,"agent":"contract-admin"}`),
	postCase("discussion join unknown", discussionJoinPath, "agent-a-session", `{"discussion_id":999}`),
	postCase("discussion join without an id", discussionJoinPath, "agent-a-session", `{"agent":"agent-a"}`),
	postCase("discussion join as an admin for an unknown agent", discussionJoinPath, "admin-web", `{"discussion_id":101,"agent":"nobody-here"}`),
	postCase("discussion join agent mismatch", discussionJoinPath, "agent-b-session", `{"discussion_id":101,"agent":"agent-a"}`),
	{
		Name: "discussion join again after leaving",
		Steps: []Step{
			discussionPost(discussionLeavePath, "agent-b-session", `{"discussion_id":101}`),
			settleStep,
			discussionPost(discussionJoinPath, "agent-b-session", `{"discussion_id":101}`),
		},
	},
}

var discussionDeferCases = []Scenario{
	postCase("discussion defer", discussionDeferPath, "agent-b-session", `{"discussion_id":`+waitingDiscussion+`}`),
	postCase("discussion defer at the limit", discussionDeferPath, "agent-b-session", `{"discussion_id":`+pastTimeoutRequired+`}`),
	postCase("discussion defer when joined", discussionDeferPath, "agent-a-session", `{"discussion_id":`+waitingDiscussion+`}`),
	postCase("discussion defer a concluded discussion", discussionDeferPath, "agent-a-session", `{"discussion_id":`+concludedDiscussion+`}`),
	postCase("discussion defer unknown", discussionDeferPath, "agent-a-session", `{"discussion_id":999}`),
	postCase("discussion defer not a participant", discussionDeferPath, "admin-web", `{"discussion_id":`+waitingDiscussion+`,"agent":"contract-admin"}`),
	postCase("discussion defer without an id", discussionDeferPath, "agent-b-session", `{}`),
	{
		// A deferred participant can defer again, and pending lists the
		// discussion under deferred_discussions. The Until step requires
		// both defers to have been written, so the case cannot pass by
		// failing the same way on both sides.
		Name: "discussion defer twice, then pending",
		Steps: []Step{
			discussionPost(discussionDeferPath, "agent-b-session", `{"discussion_id":`+waitingDiscussion+`}`),
			settleStep,
			discussionPost(discussionDeferPath, "agent-b-session", `{"discussion_id":`+waitingDiscussion+`}`),
			{Settle: true, Until: func(ctx context.Context, t *Target) (bool, error) {
				return t.Holds(ctx, `SELECT EXISTS (SELECT 1 FROM discussion_participants dp JOIN actors a ON a.id = dp.actor_id
                     WHERE dp.discussion_id = `+waitingDiscussion+` AND a.name = 'agent-b'
                       AND dp.status = 'deferred' AND dp.defer_count = 2 AND dp.deferred_at IS NOT NULL)`)
			}},
			discussionPost(discussionPendingPath, "agent-b-session", `{}`),
		},
	},
}

var discussionLeaveCases = []Scenario{
	postCase("discussion leave", discussionLeavePath, "agent-b-session", `{"discussion_id":101}`),
	postCase("discussion leave by string id", discussionLeavePath, "agent-a-key", `{"discussion_id":"`+asyncDiscussion+`"}`),
	postCase("discussion leave not a participant", discussionLeavePath, "agent-a-session", `{"discussion_id":999}`),
	postCase("discussion leave as an admin for an unknown agent", discussionLeavePath, "admin-web", `{"discussion_id":101,"agent":"nobody-here"}`),
	postCase("discussion leave without an id", discussionLeavePath, "agent-b-session", `{}`),
	postCase("discussion leave id not a number", discussionLeavePath, "agent-b-session", `{"discussion_id":"abc"}`),
}

var discussionProposeCases = []Scenario{
	postCase("discussion vote propose", discussionProposePath, "agent-a-session",
		`{"discussion_id":101,"question":"Ship it? 1=yes 2=no"}`, proposeRace...),
	postCase("discussion vote propose conclude with a close time", discussionProposePath, "agent-b-session",
		`{"discussion_id":"`+asyncDiscussion+`","question":"Wrap up?","type":"conclude","threshold":"majority","closes_at":"2099-06-01T00:00:00Z"}`, proposeRace...),
	postCase("discussion vote propose in a waiting discussion", discussionProposePath, "agent-a-session",
		`{"discussion_id":`+waitingDiscussion+`,"question":"Early question","closes_at":""}`, proposeRace...),
	postCase("discussion vote propose bad type", discussionProposePath, "agent-a-session", `{"discussion_id":101,"question":"q","type":"poll"}`),
	postCase("discussion vote propose type a number", discussionProposePath, "agent-a-session", `{"discussion_id":101,"question":"q","type":7}`),
	postCase("discussion vote propose bad threshold", discussionProposePath, "agent-a-session", `{"discussion_id":101,"question":"q","threshold":"most"}`),
	postCase("discussion vote propose bad close time", discussionProposePath, "agent-a-session", `{"discussion_id":101,"question":"q","closes_at":"soon"}`),
	postCase("discussion vote propose not joined", discussionProposePath, "agent-b-session", `{"discussion_id":`+waitingDiscussion+`,"question":"q"}`),
	postCase("discussion vote propose in a concluded discussion", discussionProposePath, "agent-a-session", `{"discussion_id":`+concludedDiscussion+`,"question":"q"}`),
	postCase("discussion vote propose not a participant", discussionProposePath, "agent-a-session", `{"discussion_id":999,"question":"q"}`),
	postCase("discussion vote propose as an admin for an unknown agent", discussionProposePath, "admin-web",
		`{"discussion_id":101,"proposed_by":"nobody-here","question":"q"}`),
	postCase("discussion vote propose without a question", discussionProposePath, "agent-a-session", `{"discussion_id":101}`),
	postCase("discussion vote propose question sanitized to nothing", discussionProposePath, "agent-a-session", `{"discussion_id":101,"question":"\u0000"}`),
	postCase("discussion vote propose proposed_by mismatch", discussionProposePath, "agent-a-session",
		`{"discussion_id":101,"proposed_by":"agent-b","question":"q"}`),
	postCase("discussion vote propose question a number", discussionProposePath, "agent-a-session", `{"discussion_id":101,"question":42}`,
		notAStringWithDiscussionRows("POST "+discussionProposePath, "discussion_votes*", "chat_message*")...),
}

var discussionCastCases = []Scenario{
	postCase("discussion vote cast closes the vote", discussionCastPath, "agent-b-session",
		`{"vote_id":`+unanimousVote+`,"choice":1,"reason":" Agreed\u0000 "}`),
	postCase("discussion vote cast keeps it open", discussionCastPath, "agent-a-session", `{"vote_id":"`+concludeVote+`","choice":1}`),
	{
		Name: "discussion vote cast concludes the discussion",
		Steps: []Step{
			discussionPost(discussionCastPath, "agent-a-session", `{"vote_id":`+concludeVote+`,"choice":1}`),
			settleStep,
			discussionPost(discussionCastPath, "agent-b-session", `{"vote_id":`+concludeVote+`,"choice":1.0,"reason":"Done here."}`),
		},
	},
	postCase("discussion vote cast an expired vote", discussionCastPath, "agent-b-session", `{"vote_id":`+expiredVote+`,"choice":0}`),
	postCase("discussion vote cast already voted", discussionCastPath, "agent-a-session", `{"vote_id":`+unanimousVote+`,"choice":2}`),
	postCase("discussion vote cast a closed vote", discussionCastPath, "agent-a-session", `{"vote_id":`+closedVote+`,"choice":1}`),
	postCase("discussion vote cast unknown vote", discussionCastPath, "agent-a-session", `{"vote_id":999,"choice":1}`),
	postCase("discussion vote cast without choice", discussionCastPath, "agent-b-session", `{"vote_id":`+unanimousVote+`}`),
	postCase("discussion vote cast choice null", discussionCastPath, "agent-b-session", `{"vote_id":`+unanimousVote+`,"choice":null}`),
	postCase("discussion vote cast choice a string", discussionCastPath, "agent-b-session", `{"vote_id":`+unanimousVote+`,"choice":"1"}`),
	postCase("discussion vote cast choice a fraction", discussionCastPath, "agent-b-session", `{"vote_id":`+unanimousVote+`,"choice":1.5}`),
	postCase("discussion vote cast choice too large for a number", discussionCastPath, "agent-b-session", `{"vote_id":`+unanimousVote+`,"choice":1e400}`),
	postCase("discussion vote cast choice past int4", discussionCastPath, "agent-b-session", `{"vote_id":`+unanimousVote+`,"choice":3000000000}`),
	postCase("discussion vote cast without vote id", discussionCastPath, "agent-b-session", `{"choice":1}`),
	postCase("discussion vote cast vote id not a number", discussionCastPath, "agent-b-session", `{"vote_id":"abc","choice":1}`),
	postCase("discussion vote cast as an admin not a participant", discussionCastPath, "admin-web",
		`{"vote_id":`+unanimousVote+`,"agent":"contract-admin","choice":1}`),
	postCase("discussion vote cast as an admin for an unknown agent", discussionCastPath, "admin-web",
		`{"vote_id":`+unanimousVote+`,"agent":"nobody-here","choice":1}`),
	postCase("discussion vote cast as an admin without agent", discussionCastPath, "admin-web", `{"vote_id":`+unanimousVote+`,"choice":1}`),
	postCase("discussion vote cast in a realtime discussion", discussionCastPath, "agent-b-session", `{"vote_id":`+realtimeVote+`,"choice":1}`),
	postCase("discussion vote cast agent mismatch", discussionCastPath, "agent-b-session", `{"vote_id":`+unanimousVote+`,"agent":"agent-a","choice":1}`),
	postCase("discussion vote cast reason a number", discussionCastPath, "agent-b-session", `{"vote_id":`+unanimousVote+`,"choice":1,"reason":5}`,
		notAStringWithDiscussionRows("POST "+discussionCastPath, "discussion_ballots*", "discussion_votes*", "chat_message*")...),
}

var discussionVoteStatusCases = []Scenario{
	postCase("discussion vote status", discussionVoteStatusPath, "agent-a-session", `{"vote_id":`+closedVote+`}`),
	postCase("discussion vote status open", discussionVoteStatusPath, "agent-b-session", `{"vote_id":"`+unanimousVote+`"}`),
	postCase("discussion vote status closes an expired vote", discussionVoteStatusPath, "admin-web", `{"vote_id":`+expiredVote+`}`),
	postCase("discussion vote status unknown", discussionVoteStatusPath, "agent-a-session", `{"vote_id":999}`),
	postCase("discussion vote status without an id", discussionVoteStatusPath, "agent-a-session", `{}`),
	postCase("discussion vote status id not a number", discussionVoteStatusPath, "agent-a-session", `{"vote_id":"abc"}`),
	{
		Name: "discussion vote status of a non-participant",
		Steps: []Step{
			{Method: "POST", Path: discussionCreatePath, Auth: "admin-web", Header: jsonType,
				Body:    `{"topic":"Admin vote","created_by":"contract-admin","participants":["contract-admin","agent-a"]}`,
				Capture: map[string]string{"id": "discussion.id"}},
			settleStep,
			{Method: "POST", Path: discussionProposePath, Auth: "admin-web", Header: jsonType,
				Body:    `{"discussion_id":{{id}},"proposed_by":"contract-admin","question":"Private?"}`,
				Capture: map[string]string{"vote": "vote.id"}},
			settleStep,
			discussionPost(discussionVoteStatusPath, "agent-b-session", `{"vote_id":{{vote}}}`),
		},
	},
}

func discussionNoBody(path string) Scenario {
	return one("discussion no JSON body "+path, Step{Method: "POST", Path: path, Auth: "agent-a-session"})
}

var discussionRoutingCases = []Scenario{
	discussionNoBody(discussionCreatePath), discussionNoBody(discussionListPath), discussionNoBody(discussionStatusPath),
	discussionNoBody(discussionPendingPath), discussionNoBody(discussionConcludePath), discussionNoBody(discussionCancelPath),
	discussionNoBody(discussionJoinPath), discussionNoBody(discussionDeferPath), discussionNoBody(discussionLeavePath),
	discussionNoBody(discussionProposePath), discussionNoBody(discussionCastPath), discussionNoBody(discussionVoteStatusPath),
	one("discussion create OPTIONS", Step{Method: "OPTIONS", Path: discussionCreatePath, Auth: "agent-a-session"}),
	one("discussion vote cast OPTIONS", Step{Method: "OPTIONS", Path: discussionCastPath, Auth: "agent-a-session"}),
	one("discussion list GET falls through", Step{Method: "GET", Path: discussionListPath, Auth: "agent-a-session"}),
	one("discussion list encoded path", Step{Method: "POST", Path: "/v1/%64iscussion/list", Auth: "agent-a-session", Header: jsonType, Body: `{}`}),
	one("discussion vote status encoded path", Step{Method: "POST", Path: "/v1/discussion/vote%2Fstatus", Auth: "agent-a-session", Header: jsonType, Body: `{"vote_id":201}`}),
	one("discussion list as a JSON array", Step{Method: "POST", Path: discussionListPath, Auth: "agent-a-session", Header: jsonType, Body: `[]`}),
	one("discussion list without credentials", Step{Method: "POST", Path: discussionListPath, Header: jsonType, Body: `{}`}),
}
