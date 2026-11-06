package contract

import "strings"

// chatCases cover the /v1/chat routes against the fixture chat
// (fixture_chat.go): fixture deliveries 1 to 4 are to agent-a (2 acked, 3 in
// the discussion, 4 a tool turn from helper-va), 5 and 6 to agent-b (6
// unsent). The chats with a virtual agent the sender may use are in
// cases_va_chat.go.
var chatCases = concat(chatSendCases, chatValidationCases, chatToolCases, chatVirtualAgentCases, chatReadCases, chatRoutingCases)

const (
	chatSendPath    = "/v1/chat/send"
	chatReceivePath = "/v1/chat/receive"
	chatAckPath     = "/v1/chat/ack"
	chatStatusPath  = "/v1/chat/status"

	// fixtureSceneID is the scene of fixture text 4, whose scene_structure
	// is "Tavern".
	fixtureSceneID = "6d2a0c1e-0000-4000-8000-0000000000e1"
	// fixtureDiscussion is fixtureDiscussionID as request text.
	fixtureDiscussion = "101"
)

// notAStringWithRows is notAStringInAdminSession for a send Node completed:
// it also wrote the chat rows Go never writes.
func notAStringWithRows(label string) []KnownDifference {
	return append(notAStringInAdminSession(label),
		KnownDifference{Where: "table chat_message*", Reason: "Node stored the message Go refused (go-server note, Known differences)"})
}

func chatSend(name, auth, body string, known ...KnownDifference) Scenario {
	return postCase(name, chatSendPath, auth, body, known...)
}

var chatSendCases = []Scenario{
	chatSend("chat send", "agent-a-session", `{"to_agents":["agent-b"],"message":"Hello."}`),
	chatSend("chat send with an API key", "agent-b-key", `{"to_agents":["agent-a"],"message":"Sent with a key."}`),
	chatSend("chat send names sanitized and deduplicated", "agent-a-session", `{"to_agents":[" Agent-B ","agent-b"],"message":"Once."}`),
	chatSend("chat send message control characters stripped", "agent-a-session", `{"to_agents":["agent-b"],"message":"a\u0000b‮c\td\ne"}`),
	chatSend("chat send empty message", "agent-a-session", `{"to_agents":["agent-b"],"message":""}`),
	chatSend("chat send message sanitized to nothing", "agent-a-session", `{"to_agents":["agent-b"],"message":"​"}`),
	chatSend("chat send without message", "agent-a-session", `{"to_agents":["agent-b"]}`),
	chatSend("chat send message null", "agent-a-session", `{"to_agents":["agent-b"],"message":null}`),
	chatSend("chat send without recipients", "agent-a-session", `{"message":"m"}`),
	chatSend("chat send to_agents empty", "agent-a-session", `{"to_agents":[],"message":"m"}`),
	chatSend("chat send to_agents a name string", "agent-a-session", `{"to_agents":"agent-b","message":"m"}`),
	chatSend("chat send to_agents a number", "agent-a-session", `{"to_agents":5,"message":"m"}`),
	chatSend("chat send to_agents empty string", "agent-a-session", `{"to_agents":"","message":"m"}`),
	chatSend("chat send to self only", "agent-a-session", `{"to_agents":["agent-a"],"message":"m"}`),
	chatSend("chat send to an unknown agent", "agent-a-session", `{"to_agents":["agent-b","nobody-here"],"message":"m"}`),
	chatSend("chat send star among names", "agent-a-session", `{"to_agents":["agent-b","*"],"message":"m"}`),
	chatSend("chat send name sanitized to nothing", "agent-a-session", `{"to_agents":["!!!"],"message":"m"}`),
	chatSend("chat send to everyone", "agent-a-session", `{"to_agents":["*"],"message":"Hello, all."}`),
	chatSend("chat send to everyone as a string", "agent-b-session", `{"to_agents":"*","message":"Hello, all."}`),
	chatSend("chat send from_agent mismatch", "agent-a-session", `{"from_agent":"agent-b","to_agents":["agent-b"],"message":"m"}`),
	chatSend("chat send from_agent a number", "agent-a-session", `{"from_agent":5,"to_agents":["agent-b"],"message":"m"}`),
	chatSend("chat send from_agent sanitized to the agent", "agent-a-session", `{"from_agent":" Agent-A ","to_agents":["agent-b"],"message":"m"}`),
	chatSend("chat send as an admin for an agent", "admin-web", `{"from_agent":"agent-b","to_agents":["agent-a"],"message":"On behalf."}`),
	chatSend("chat send as an admin without from_agent", "admin-web", `{"to_agents":["agent-a"],"message":"m"}`),
	chatSend("chat send as an admin from an unknown agent", "admin-web", `{"from_agent":"nobody-here","to_agents":["agent-a"],"message":"m"}`),
	chatSend("chat send message a number", "agent-a-session", `{"to_agents":["agent-b"],"message":5}`, notAStringWithRows("POST "+chatSendPath)...),
	chatSend("chat send to_agents a number in the array", "admin-web", `{"from_agent":"agent-a","to_agents":[5],"message":"m"}`, notAStringInAdminSession("POST "+chatSendPath)...),
	chatSend("chat send to system", "agent-a-session", `{"to_agents":["system"],"message":"{\"type\":\"virtual-agent\",\"discussionId\":`+fixtureDiscussion+`,\"triggerType\":\"message\"}"}`),
	chatSend("chat send to system not JSON", "agent-a-session", `{"to_agents":["system"],"message":"hello"}`),
	{
		Name: "chat send to the discussion",
		Steps: []Step{
			{Method: "POST", Path: chatSendPath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"discussion_id":` + fixtureDiscussion + `,"message":"To the room."}`},
			{Settle: true},
		},
	},
	{
		Name: "chat send to the discussion by legacy channel and to a name",
		Steps: []Step{
			{Method: "POST", Path: chatSendPath, Auth: "agent-b-session", Header: jsonType,
				Body: `{"channel":"discussion-` + fixtureDiscussion + `","to_agents":["system"],"message":"Room and system."}`},
			{Settle: true},
		},
	},
	{
		Name: "chat send discussion_id as a string",
		Steps: []Step{
			{Method: "POST", Path: chatSendPath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"discussion_id":"` + fixtureDiscussion + `","message":"m"}`},
			{Settle: true},
		},
	},
	chatSend("chat send unknown discussion with a name", "agent-a-session", `{"discussion_id":999,"to_agents":["agent-b"],"message":"m"}`),
	chatSend("chat send unknown discussion alone", "agent-a-session", `{"discussion_id":999,"message":"m"}`),
	chatSend("chat send discussion_id 0 ignores the channel", "agent-a-session", `{"discussion_id":0,"channel":"discussion-`+fixtureDiscussion+`","message":"m"}`),
	chatSend("chat send discussion_id past int4", "agent-a-session", `{"discussion_id":3000000000,"to_agents":["agent-b"],"message":"m"}`),
	chatSend("chat send channel not a discussion", "agent-a-session", `{"channel":"general","to_agents":["agent-b"],"message":"m"}`),
	// JavaScript's $ without the m flag matches only at the end, as Go's does.
	chatSend("chat send channel with a trailing newline", "agent-a-session", `{"channel":"discussion-`+fixtureDiscussion+`\n","to_agents":["agent-b"],"message":"m"}`),
	chatSend("chat send channel a number", "agent-a-session", `{"channel":5,"to_agents":["agent-b"],"message":"m"}`,
		KnownDifference{Where: "step 1 POST " + chatSendPath + ": *", Reason: "a channel that is not a string is a 400 in Go; Node's channel.match threw a TypeError, a 500 (go-server note, Known differences)"},
		KnownDifference{Where: "table error_log*", Reason: "the same refusal is logged with Go's status and message"},
		KnownDifference{Where: "table request_log*", Reason: "the request log records the status and length the client got"}),
}

var chatValidationCases = []Scenario{
	chatSend("chat send stable_context from an agent", "agent-a-session", `{"to_agents":["agent-b"],"message":"m","stable_context":"x"}`),
	chatSend("chat send ephemeral_context from an agent", "agent-a-session", `{"to_agents":["agent-b"],"message":"m","ephemeral_context":"x"}`),
	chatSend("chat send ephemeral_context null", "agent-a-session", `{"to_agents":["agent-b"],"message":"m","ephemeral_context":null}`),
	// The gate passes for an admin naming the engine, and the message goes
	// to agent-b, who is not virtual.
	chatSend("chat send ephemeral_context as the engine", "admin-web", `{"from_agent":"salem-engine","to_agents":["agent-b"],"message":"m","ephemeral_context":"x","stable_context":"y"}`),
	chatSend("chat send stable_context not a string", "admin-web", `{"from_agent":"salem-engine","to_agents":["agent-b"],"message":"m","stable_context":5}`),
	chatSend("chat send stable_context too long", "admin-web", `{"from_agent":"salem-engine","to_agents":["agent-b"],"message":"m","stable_context":"`+strings.Repeat("x", 65537)+`"}`),
	chatSend("chat send tool_calls not an array", "agent-a-session", `{"to_agents":["agent-b"],"message":"m","tool_calls":{}}`),
	chatSend("chat send tools_offered not an array", "agent-a-session", `{"to_agents":["agent-b"],"message":"m","tools_offered":"x"}`),
	chatSend("chat send tool_call_id not a string", "agent-a-session", `{"to_agents":["agent-b"],"message":"m","tool_call_id":5}`),
	chatSend("chat send tool_call_results not an array", "agent-a-session", `{"to_agents":["agent-b"],"tool_call_results":{}}`),
	chatSend("chat send tool_call_results empty", "agent-a-session", `{"to_agents":["agent-b"],"tool_call_results":[]}`),
	chatSend("chat send tool_call_results bad entry", "agent-a-session", `{"to_agents":["agent-b"],"tool_call_results":[{"id":"c1","content":5}]}`),
	chatSend("chat send tool_call_results with tool_call_id", "agent-a-session", `{"to_agents":["agent-b"],"tool_call_results":[{"id":"c1","content":"x"}],"tool_call_id":"c2"}`),
	chatSend("chat send tool_call_results with message", "agent-a-session", `{"to_agents":["agent-b"],"tool_call_results":[{"id":"c1","content":"x"}],"message":"m"}`),
	chatSend("chat send persist_only without results", "agent-a-session", `{"to_agents":["agent-b"],"message":"m","persist_only":true}`),
	chatSend("chat send persist_only with wait", "agent-a-session", `{"to_agents":["agent-b"],"tool_call_results":[{"id":"c1","content":"x"}],"persist_only":true,"wait":true}`),
	chatSend("chat send scene_id not a uuid", "agent-a-session", `{"to_agents":["agent-b"],"message":"m","scene_id":"scene-1"}`),
	chatSend("chat send scene_structure too long", "agent-a-session", `{"to_agents":["agent-b"],"message":"m","scene_structure":"`+strings.Repeat("s", 101)+`"}`),
	chatSend("chat send conversation_id too long before the trim", "agent-a-session", `{"to_agents":["agent-b"],"message":"m","conversation_id":" `+strings.Repeat("c", 64)+`"}`),
	chatSend("chat send sim_actor_id not a string", "agent-a-session", `{"to_agents":["agent-b"],"message":"m","sim_actor_id":5}`),
	chatSend("chat send sim_actor_name too long", "agent-a-session", `{"to_agents":["agent-b"],"message":"m","sim_actor_name":"`+strings.Repeat("n", 101)+`"}`),
	chatSend("chat send history_scope not scene", "agent-a-session", `{"to_agents":["agent-b"],"message":"m","history_scope":"pair"}`),
	chatSend("chat send wait without a virtual recipient", "agent-a-session", `{"to_agents":["agent-b"],"message":"m","wait":true}`),
	chatSend("chat send wait not exactly true", "agent-a-session", `{"to_agents":["agent-b"],"message":"m","wait":"true"}`),
}

var chatToolCases = []Scenario{
	chatSend("chat send tool fields", "agent-a-session",
		`{"to_agents":["agent-b"],"message":"m","tool_calls":[{"z":1,"a":{"y":2,"b":3},"1":0}],"tool_call_id":"c1","tools_offered":[{"name":"speak"}]}`),
	chatSend("chat send tool_call_id empty", "agent-a-session", `{"to_agents":["agent-b"],"message":"m","tool_call_id":""}`),
	chatSend("chat send tool_call_results", "agent-a-session",
		`{"to_agents":["agent-b"],"tool_call_results":[{"id":"c1","content":"one"},{"id":"c2","content":"two"}],"tool_calls":[{"id":"c1"}],"tools_offered":[]}`),
	chatSend("chat send tool_call_results to two agents, persist only", "admin-web",
		`{"from_agent":"agent-a","to_agents":["agent-b","system"],"tool_call_results":[{"id":"c1","content":"one"},{"id":"c2","content":""}],"message":"","persist_only":true}`),
	chatSend("chat send scene fields", "agent-a-session",
		`{"to_agents":["agent-b"],"message":"m","scene_id":"6D2A0C1E-0000-4000-8000-0000000000F1","scene_structure":"Forge","conversation_id":"  sc-9  ","sim_actor_id":" npc-1 ","sim_actor_name":"   ","history_scope":"scene"}`),
	chatSend("chat send scene structure inherited", "agent-a-session",
		`{"to_agents":["agent-b"],"message":"m","scene_id":"`+fixtureSceneID+`"}`),
	chatSend("chat send blank scene structure", "agent-a-session",
		`{"to_agents":["agent-b"],"message":"m","scene_id":"`+fixtureSceneID+`","scene_structure":"   ","conversation_id":"   "}`),
	chatSend("chat send scene structure kept as sent", "agent-a-session",
		`{"to_agents":["agent-b"],"message":"m","scene_structure":" Inn "}`),
}

var chatVirtualAgentCases = []Scenario{
	chatSend("chat send to a virtual agent without access", "agent-a-session", `{"to_agents":["helper-va"],"message":"Can you help?"}`),
	chatSend("chat send to a virtual agent and another agent", "agent-a-session", `{"to_agents":["helper-va","agent-b"],"message":"Both of you."}`),
	chatSend("chat send wait to a virtual agent without access", "agent-a-session", `{"to_agents":["helper-va"],"message":"m","wait":true}`),
	chatSend("chat send tool results to a virtual agent without access", "agent-a-session",
		`{"to_agents":["helper-va"],"tool_call_results":[{"id":"c1","content":"x"}]}`),
	chatSend("chat send persist only to a virtual agent", "agent-a-session",
		`{"to_agents":["helper-va"],"tool_call_results":[{"id":"c1","content":"x"}],"persist_only":true}`),
}

var chatReadCases = []Scenario{
	postCase("chat receive", chatReceivePath, "agent-a-session", `{}`),
	postCase("chat receive in the discussion", chatReceivePath, "agent-a-session", `{"discussion_id":`+fixtureDiscussion+`}`),
	postCase("chat receive by legacy channel", chatReceivePath, "agent-a-session", `{"channel":"discussion-`+fixtureDiscussion+`"}`),
	postCase("chat receive from one sender", chatReceivePath, "agent-a-session", `{"from_agent":"Helper-VA"}`),
	postCase("chat receive after an id", chatReceivePath, "agent-a-session", `{"after_id":1}`),
	postCase("chat receive after_id 0", chatReceivePath, "agent-a-session", `{"after_id":0}`),
	postCase("chat receive after_id negative", chatReceivePath, "agent-a-session", `{"after_id":-1}`),
	postCase("chat receive after_id a string", chatReceivePath, "agent-a-session", `{"after_id":"1"}`),
	postCase("chat receive after_id null", chatReceivePath, "agent-a-session", `{"after_id":null}`),
	postCase("chat receive after_id a fraction", chatReceivePath, "agent-a-session", `{"after_id":1.5}`),
	postCase("chat receive after_id past int4", chatReceivePath, "agent-a-session", `{"after_id":100000000000000000000}`),
	postCase("chat receive from an unknown agent", chatReceivePath, "agent-a-session", `{"from_agent":"nobody-here"}`),
	postCase("chat receive mismatch", chatReceivePath, "agent-b-session", `{"agent":"agent-a"}`),
	postCase("chat receive as an admin", chatReceivePath, "admin-web", `{"agent":"agent-b"}`),
	postCase("chat receive as an admin without agent", chatReceivePath, "admin-web", `{}`),
	postCase("chat receive unknown agent", chatReceivePath, "admin-web", `{"agent":"nobody-here"}`),
	postCase("chat receive an array body", chatReceivePath, "agent-a-session", `[]`),
	postCase("chat ack", chatAckPath, "agent-a-session", `{"ids":[1,2,5]}`),
	postCase("chat ack legacy message_ids", chatAckPath, "agent-a-session", `{"message_ids":[1]}`),
	postCase("chat ack ids as strings", chatAckPath, "agent-a-session", `{"ids":["1","4"]}`),
	postCase("chat ack empty", chatAckPath, "agent-a-session", `{"ids":[]}`),
	postCase("chat ack ids not an array", chatAckPath, "agent-a-session", `{"ids":1}`),
	postCase("chat ack a non-number", chatAckPath, "agent-a-session", `{"ids":["x"]}`),
	postCase("chat ack a null", chatAckPath, "agent-a-session", `{"ids":[1,null]}`),
	postCase("chat ack mismatch", chatAckPath, "agent-b-session", `{"agent":"agent-a","ids":[1]}`),
	postCase("chat ack as an admin", chatAckPath, "admin-web", `{"agent":"agent-b","ids":[5,6]}`),
	postCase("chat status", chatStatusPath, "agent-a-session", `{}`),
	postCase("chat status in the discussion", chatStatusPath, "agent-a-session", `{"discussion_id":"`+fixtureDiscussion+`"}`),
	postCase("chat status with nothing", chatStatusPath, "agent-b-session", `{"discussion_id":`+fixtureDiscussion+`}`),
	postCase("chat status as an admin", chatStatusPath, "admin-web", `{"agent":"agent-b"}`),
	postCase("chat status as an admin without agent", chatStatusPath, "admin-web", `{}`),
	postCase("chat status mismatch", chatStatusPath, "agent-a-session", `{"agent":"agent-b"}`),
	{
		Name: "chat send, receive, ack, then status",
		Steps: []Step{
			{Method: "POST", Path: chatSendPath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"to_agents":["agent-b"],"message":"Round trip."}`, Capture: map[string]string{"delivery": "to_agents.0.id"}},
			{Method: "POST", Path: chatStatusPath, Auth: "agent-b-session", Header: jsonType, Body: `{}`},
			{Method: "POST", Path: chatReceivePath, Auth: "agent-b-session", Header: jsonType, Body: `{"from_agent":"agent-a"}`},
			{Method: "POST", Path: chatAckPath, Auth: "agent-b-session", Header: jsonType, Body: `{"ids":[{{delivery}}]}`},
			{Method: "POST", Path: chatAckPath, Auth: "agent-b-session", Header: jsonType, Body: `{"ids":[{{delivery}}]}`},
			{Method: "POST", Path: chatReceivePath, Auth: "agent-b-session", Header: jsonType, Body: `{}`},
			{Method: "POST", Path: chatStatusPath, Auth: "agent-b-session", Header: jsonType, Body: `{}`},
		},
	},
}

// chatNoBody sends text/plain, so neither server parses a JSON body.
func chatNoBody(path string) Scenario {
	return one("chat "+path[len("/v1/chat/"):]+" without a JSON body", Step{Method: "POST", Path: path, Auth: "agent-a-session",
		Header: [][2]string{{"Content-Type", "text/plain"}}, Body: "x"})
}

var chatRoutingCases = []Scenario{
	chatNoBody(chatSendPath), chatNoBody(chatReceivePath), chatNoBody(chatAckPath), chatNoBody(chatStatusPath),
	one("chat send OPTIONS", Step{Method: "OPTIONS", Path: chatSendPath, Auth: "agent-a-session"}),
	one("chat status GET falls through", Step{Method: "GET", Path: chatStatusPath, Auth: "agent-a-session"}),
	one("chat send encoded path", Step{Method: "POST", Path: "/v1/%63hat/send", Auth: "agent-a-session",
		Header: jsonType, Body: `{"to_agents":["agent-b"],"message":"m"}`}),
	one("chat receive without credentials", Step{Method: "POST", Path: chatReceivePath, Header: jsonType, Body: `{}`}),
}
