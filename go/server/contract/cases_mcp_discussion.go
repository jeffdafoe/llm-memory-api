package contract

// The discussion and vote tools over the discussion fixture (ids in
// cases_discussion.go). agent-a holds every mcp_* permission; agent-b gets
// the discussion tools from grantAgentBDiscussions.

// grantAgentBDiscussions gives agent-b the discussion tools' permissions.
var grantAgentBDiscussions = Step{SQL: `INSERT INTO agent_permissions (actor_id, permission_id)
    SELECT a.id, p.id FROM actors a, permissions p
    WHERE a.name = 'agent-b' AND p.name LIKE 'mcp_discussion_%'`}

// stampedAs makes the caller's MCP last_seen stamp land first: the
// participants query reads actors in heap order, and a stamp moves the
// caller's row; once it is the newest, a later stamp keeps it last.
func stampedAs(name, auth string, steps ...Step) Scenario {
	first := []Step{grantAgentBDiscussions, mcpStep(auth, rpc(1, "tools/list", "")), {Settle: true}}
	return Scenario{Name: name, Steps: append(first, steps...)}
}

func discussionTool(auth string, id int, tool, args string) Step {
	return mcpStep(auth, call(id, tool, args))
}

// discussionToolCase is one tool call after the caller's stamp.
func discussionToolCase(name, auth, tool, args string, known ...KnownDifference) Scenario {
	sc := stampedAs(name, auth, discussionTool(auth, 2, tool, args))
	sc.Known = known
	return sc
}

const (
	mcpA = "agent-a-mcp"
	mcpB = "agent-b-mcp"
)

var mcpDiscussionCases = []Scenario{
	// Create.
	discussionToolCase("mcp discussion_create", mcpA, "discussion_create",
		`{"topic":" MCP topic ","participants":["agent-a"," Agent-B "],"mode":"async","context":"ctx\u0000 here"}`),
	discussionToolCase("mcp discussion_create default mode", mcpA, "discussion_create",
		`{"topic":"Plain","participants":["agent-a","agent-b"],"mode":"nonsense","context":""}`),
	stampedAs("mcp discussion_create conflict", mcpA,
		discussionTool(mcpA, 2, "discussion_status", `{"discussion_id":`+pastTimeoutRequired+`}`),
		discussionTool(mcpA, 3, "discussion_create", `{"topic":"Blocked","participants":["agent-a","agent-b"]}`)),
	discussionToolCase("mcp discussion_create created_by mismatch", mcpA, "discussion_create", `{"topic":"t","participants":["agent-a","agent-b"],"created_by":"agent-b"}`),
	discussionToolCase("mcp discussion_create one participant", mcpA, "discussion_create", `{"topic":"t","participants":["agent-a"]}`),
	discussionToolCase("mcp discussion_create unknown participant", mcpA, "discussion_create", `{"topic":"t","participants":["agent-a","nobody-here"]}`),
	discussionToolCase("mcp discussion_create creator not a participant", mcpA, "discussion_create", `{"topic":"t","participants":["agent-b","helper-va"]}`),
	discussionToolCase("mcp discussion_create topic sanitized to nothing", mcpA, "discussion_create", `{"topic":"​","participants":["agent-a","agent-b"]}`),

	// List: Node passed args.agent as it was, so any agent's list.
	discussionToolCase("mcp discussion_list", mcpA, "discussion_list", `{}`),
	discussionToolCase("mcp discussion_list by status", mcpA, "discussion_list", `{"status":"concluded"}`),
	discussionToolCase("mcp discussion_list for another agent", mcpA, "discussion_list", `{"agent":"agent-b","status":"waiting"}`),
	discussionToolCase("mcp discussion_list unknown agent", mcpA, "discussion_list", `{"agent":"nobody-here"}`),

	// Status.
	discussionToolCase("mcp discussion_status", mcpA, "discussion_status", `{"discussion_id":101}`),
	discussionToolCase("mcp discussion_status closes an expired vote", mcpB, "discussion_status", `{"discussion_id":`+asyncDiscussion+`}`),
	discussionToolCase("mcp discussion_status times out", mcpA, "discussion_status", `{"discussion_id":`+pastTimeoutRequired+`}`),
	discussionToolCase("mcp discussion_status concluded", mcpA, "discussion_status", `{"discussion_id":`+concludedDiscussion+`}`),
	discussionToolCase("mcp discussion_status with context", mcpA, "discussion_status", `{"discussion_id":110}`),
	discussionToolCase("mcp discussion_status unknown", mcpA, "discussion_status", `{"discussion_id":999}`),
	discussionToolCase("mcp discussion_status fractional id", mcpA, "discussion_status", `{"discussion_id":101.5}`),

	// Pending.
	discussionToolCase("mcp discussion_pending", mcpA, "discussion_pending", `{}`),
	discussionToolCase("mcp discussion_pending of an invitee", mcpB, "discussion_pending", `{}`),
	discussionToolCase("mcp discussion_pending identity mismatch", mcpA, "discussion_pending", `{"agent":"agent-b"}`),

	// Conclude and cancel.
	discussionToolCase("mcp discussion_conclude", mcpA, "discussion_conclude", `{"discussion_id":`+asyncDiscussion+`}`, deliveryRace...),
	discussionToolCase("mcp discussion_conclude not joined", mcpB, "discussion_conclude", `{"discussion_id":`+waitingDiscussion+`}`),
	discussionToolCase("mcp discussion_conclude after leaving", mcpA, "discussion_conclude", `{"discussion_id":`+concludedDiscussion+`}`),
	discussionToolCase("mcp discussion_conclude unknown", mcpA, "discussion_conclude", `{"discussion_id":999}`),
	discussionToolCase("mcp discussion_conclude agent mismatch", mcpA, "discussion_conclude", `{"discussion_id":101,"agent":"agent-b"}`),
	discussionToolCase("mcp discussion_cancel", mcpB, "discussion_cancel", `{"discussion_id":`+asyncDiscussion+`}`, deliveryRace...),
	discussionToolCase("mcp discussion_cancel a waiting discussion", mcpA, "discussion_cancel", `{"discussion_id":`+waitingDiscussion+`}`, deliveryRace...),

	// Join, leave, defer.
	discussionToolCase("mcp discussion_join makes it active", mcpB, "discussion_join", `{"discussion_id":`+waitingDiscussion+`}`),
	discussionToolCase("mcp discussion_join already joined", mcpA, "discussion_join", `{"discussion_id":101}`),
	discussionToolCase("mcp discussion_join a concluded discussion", mcpB, "discussion_join", `{"discussion_id":`+concludedDiscussion+`}`),
	discussionToolCase("mcp discussion_join unknown", mcpA, "discussion_join", `{"discussion_id":999}`),
	discussionToolCase("mcp discussion_leave", mcpB, "discussion_leave", `{"discussion_id":101}`),
	discussionToolCase("mcp discussion_leave not a participant", mcpA, "discussion_leave", `{"discussion_id":999}`),
	discussionToolCase("mcp discussion_defer", mcpB, "discussion_defer", `{"discussion_id":`+waitingDiscussion+`}`),
	discussionToolCase("mcp discussion_defer at the limit", mcpB, "discussion_defer", `{"discussion_id":`+pastTimeoutRequired+`}`),
	discussionToolCase("mcp discussion_defer when joined", mcpA, "discussion_defer", `{"discussion_id":`+waitingDiscussion+`}`),

	// Votes.
	discussionToolCase("mcp discussion_vote_propose", mcpA, "discussion_vote_propose", `{"discussion_id":101,"question":"Ship it?\u0000","type":"general","threshold":"majority"}`, proposeRace...),
	discussionToolCase("mcp discussion_vote_propose with a close time", mcpB, "discussion_vote_propose", `{"discussion_id":`+asyncDiscussion+`,"question":"Done?","type":"conclude","closes_at":"2099-01-01T00:00:00Z"}`, proposeRace...),
	discussionToolCase("mcp discussion_vote_propose bad type", mcpA, "discussion_vote_propose", `{"discussion_id":101,"question":"q","type":"poll"}`),
	discussionToolCase("mcp discussion_vote_propose bad close time", mcpA, "discussion_vote_propose", `{"discussion_id":101,"question":"q","closes_at":"soon"}`),
	discussionToolCase("mcp discussion_vote_propose in a concluded discussion", mcpA, "discussion_vote_propose", `{"discussion_id":`+concludedDiscussion+`,"question":"q"}`),
	discussionToolCase("mcp discussion_vote_propose identity mismatch", mcpA, "discussion_vote_propose", `{"discussion_id":101,"question":"q","proposed_by":"agent-b"}`),
	discussionToolCase("mcp discussion_vote_cast", mcpA, "discussion_vote_cast", `{"vote_id":`+realtimeVote+`,"choice":2,"reason":"Because\u0000."}`),
	discussionToolCase("mcp discussion_vote_cast without a reason", mcpB, "discussion_vote_cast", `{"vote_id":`+unanimousVote+`,"choice":1}`),
	discussionToolCase("mcp discussion_vote_cast on a closed vote", mcpA, "discussion_vote_cast", `{"vote_id":`+closedVote+`,"choice":1}`),
	discussionToolCase("mcp discussion_vote_cast fractional choice", mcpA, "discussion_vote_cast", `{"vote_id":`+realtimeVote+`,"choice":1.5}`),
	discussionToolCase("mcp discussion_vote_cast unknown vote", mcpA, "discussion_vote_cast", `{"vote_id":999,"choice":1}`),
	discussionToolCase("mcp discussion_vote_status", mcpA, "discussion_vote_status", `{"vote_id":`+closedVote+`}`),
	discussionToolCase("mcp discussion_vote_status no ballots", mcpA, "discussion_vote_status", `{"vote_id":`+concludeVote+`}`),
	discussionToolCase("mcp discussion_vote_status closes an expired vote", mcpA, "discussion_vote_status", `{"vote_id":`+expiredVote+`}`),
	discussionToolCase("mcp discussion_vote_status unknown", mcpA, "discussion_vote_status", `{"vote_id":999}`),
}
