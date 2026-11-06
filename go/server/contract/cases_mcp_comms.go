package contract

// The chat, mail, agent and activity tools, as agent-a (which holds every
// mcp_* permission) over the fixture's chat and mail.

// stamped makes agent-a's MCP last_seen stamp land before a roster read:
// every MCP request stamps it in the background, and agent_status orders by
// it.
func stamped(name string, steps ...Step) Scenario {
	first := []Step{mcpStep("agent-a-mcp", rpc(1, "tools/list", "")), {Settle: true}}
	return Scenario{Name: name, Steps: append(first, steps...)}
}

var mcpCommsCases = []Scenario{
	// Chat.
	toolCase("mcp chat_send", "agent-a-mcp", "chat_send", `{"to":"agent-b","message":"Hello over MCP."}`),
	toolCase("mcp chat_send to everyone", "agent-a-mcp", "chat_send", `{"to":"*","message":"Hello, all."}`),
	toolCase("mcp chat_send name sanitized", "agent-a-mcp", "chat_send", `{"to":" Agent-B ","message":"a\u0000b"}`),
	toolCase("mcp chat_send in a discussion", "agent-a-mcp", "chat_send", `{"discussion_id":101,"message":"In 101."}`),
	toolCase("mcp chat_send discussion past int4", "agent-a-mcp", "chat_send", `{"discussion_id":3000000000,"message":"x"}`),
	toolCase("mcp chat_send no recipient", "agent-a-mcp", "chat_send", `{"message":"x"}`),
	toolCase("mcp chat_send unknown agent", "agent-a-mcp", "chat_send", `{"to":"nobody","message":"x"}`),
	toolCase("mcp chat_send identity mismatch", "agent-a-mcp", "chat_send", `{"to":"agent-b","from":"agent-b","message":"x"}`),
	toolCase("mcp chat_send own identity", "agent-a-mcp", "chat_send", `{"to":"agent-b","from":"agent-a","message":"x"}`),
	toolCase("mcp chat_receive", "agent-a-mcp", "chat_receive", `{}`),
	toolCase("mcp chat_receive in a discussion", "agent-a-mcp", "chat_receive", `{"discussion_id":101}`),
	toolCase("mcp chat_receive nothing", "agent-a-mcp", "chat_receive", `{"discussion_id":102}`),
	toolCase("mcp chat_receive identity mismatch", "agent-a-mcp", "chat_receive", `{"agent":"agent-b"}`),
	toolCase("mcp chat_ack", "agent-a-mcp", "chat_ack", `{"ids":[1,4,999]}`),
	toolCase("mcp chat_ack empty", "agent-a-mcp", "chat_ack", `{"ids":[]}`),
	toolCase("mcp chat_ack fractional id", "agent-a-mcp", "chat_ack", `{"ids":[1.5]}`),
	// ids is required, so Node's legacy message_ids fallback is never reached.
	toolCase("mcp chat_ack legacy message_ids", "agent-a-mcp", "chat_ack", `{"message_ids":[1,4]}`),
	toolCase("mcp chat_ack ids empty beside message_ids", "agent-a-mcp", "chat_ack", `{"ids":[],"message_ids":[1]}`),
	toolCase("mcp chat_receive lowest int64 discussion", "agent-a-mcp", "chat_receive", `{"discussion_id":-9223372036854775808}`),
	toolCase("mcp chat_status", "agent-a-mcp", "chat_status", `{}`),
	toolCase("mcp chat_status in a discussion", "agent-a-mcp", "chat_status", `{"discussion_id":101}`),
	toolCase("mcp chat_status nothing", "agent-a-mcp", "chat_status", `{"discussion_id":999}`),
	{
		Name: "mcp chat round trip",
		Steps: []Step{
			mcpStep("agent-a-mcp", call(1, "chat_receive", `{}`)),
			mcpStep("agent-a-mcp", call(2, "chat_ack", `{"ids":[1,4]}`)),
			mcpStep("agent-a-mcp", call(3, "chat_status", `{}`)),
			mcpStep("agent-a-mcp", call(4, "chat_receive", `{}`)),
		},
	},

	// Mail.
	toolCase("mcp mail_send", "agent-a-mcp", "mail_send", `{"to":"agent-b","subject":"Over MCP","body":"Body\u0007 text."}`),
	toolCase("mcp mail_send reply", "agent-a-mcp", "mail_send", `{"to":" Agent-B ","subject":"Re: x","body":"b","in_reply_to":"`+fixtureUnreadToA+`"}`),
	toolCase("mcp mail_send to a virtual agent", "agent-a-mcp", "mail_send", `{"to":"helper-va","subject":"Hi","body":"Question."}`),
	toolCase("mcp mail_send unknown agent", "agent-a-mcp", "mail_send", `{"to":"nobody","subject":"s","body":"b"}`),
	toolCase("mcp mail_send subject sanitized to nothing", "agent-a-mcp", "mail_send", `{"to":"agent-b","subject":"​","body":"b"}`),
	toolCase("mcp mail_send identity mismatch", "agent-a-mcp", "mail_send", `{"to":"agent-b","from":"agent-b","subject":"s","body":"b"}`),
	toolCase("mcp mail_check", "agent-a-mcp", "mail_check", `{}`),
	toolCase("mcp mail_receive", "agent-a-mcp", "mail_receive", `{"ids":["`+fixtureUnreadToA+`","6d2a0c1e-0000-4000-8000-0000000000c1","`+fixtureReadByA+`"]}`),
	toolCase("mcp mail_receive nothing", "agent-a-mcp", "mail_receive", `{"ids":["`+fixtureUnreadToB+`"]}`),
	toolCase("mcp mail_receive empty", "agent-a-mcp", "mail_receive", `{"ids":[]}`),
	toolCase("mcp mail_receive bad id", "agent-a-mcp", "mail_receive", `{"ids":["x"]}`),
	toolCase("mcp mail_ack", "agent-a-mcp", "mail_ack", `{"ids":["`+fixtureUnreadToA+`","`+fixtureReadByA+`"]}`),
	toolCase("mcp mail_ack bad id", "agent-a-mcp", "mail_ack", `{"ids":["x"]}`),
	toolCase("mcp mail_edit", "agent-a-mcp", "mail_edit", `{"id":"`+fixtureUnreadToB+`","subject":"Edited subject"}`),
	toolCase("mcp mail_edit body", "agent-a-mcp", "mail_edit", `{"id":"`+fixtureUnreadToB+`","body":"New body."}`),
	toolCase("mcp mail_edit no fields", "agent-a-mcp", "mail_edit", `{"id":"`+fixtureUnreadToB+`"}`),
	toolCase("mcp mail_edit acked", "agent-a-mcp", "mail_edit", `{"id":"`+fixtureReadByB+`","subject":"x"}`),
	toolCase("mcp mail_unsend", "agent-a-mcp", "mail_unsend", `{"id":"`+fixtureUnreadToB+`"}`),
	toolCase("mcp mail_unsend not found", "agent-a-mcp", "mail_unsend", `{"id":"`+fixtureUnsentToB+`"}`),
	toolCase("mcp mail_sent", "agent-a-mcp", "mail_sent", `{}`),
	toolCase("mcp mail_sent to and limit", "agent-a-mcp", "mail_sent", `{"to":"Agent-B","limit":1,"offset":1}`),
	toolCase("mcp mail_sent nothing", "agent-a-mcp", "mail_sent", `{"to":"helper-va"}`),
	toolCase("mcp mail_sent unknown peer", "agent-a-mcp", "mail_sent", `{"to":"nobody"}`),
	toolCase("mcp mail_history", "agent-a-mcp", "mail_history", `{}`),
	toolCase("mcp mail_history from", "agent-a-mcp", "mail_history", `{"from":"agent-b","limit":500}`),
	toolCase("mcp mail_history nothing", "agent-a-mcp", "mail_history", `{"from":"helper-va"}`),
	toolCase("mcp mail_history identity mismatch", "agent-a-mcp", "mail_history", `{"agent":"agent-b"}`),

	// Agent and activity.
	stamped("mcp agent_status", mcpStep("agent-a-mcp", call(2, "agent_status", `{}`))),
	stamped("mcp agent_status after a profile and expertise",
		mcpStep("agent-a-mcp", call(2, "update_profile", `{"provider":"anthropic","model":"claude-x"}`)),
		mcpStep("agent-a-mcp", call(3, "activity_start", `{}`)),
		mcpStep("agent-a-mcp", call(4, "agent_status", `{}`))),
	stamped("mcp agent_status odd expertise",
		Step{SQL: `UPDATE actors SET expertise = '[null, 1, "x", [1, 2]]' WHERE name = 'agent-b'`},
		mcpStep("agent-a-mcp", call(2, "agent_status", `{}`))),
	stamped("mcp agent_status expertise a string",
		Step{SQL: `UPDATE actors SET expertise = '"go"' WHERE name = 'agent-b'`},
		mcpStep("agent-a-mcp", call(2, "agent_status", `{}`))),
	toolCase("mcp agent_status identity mismatch", "agent-a-mcp", "agent_status", `{"agent":"agent-b"}`),
	toolCase("mcp update_expertise", "agent-a-mcp", "update_expertise", `{"expertise":[" Go ","","  ","RUST\u0000","Ünïcode"]}`),
	toolCase("mcp update_expertise empty", "agent-a-mcp", "update_expertise", `{"expertise":[]}`),
	toolCase("mcp update_profile", "agent-a-mcp", "update_profile", `{"provider":" open<ai> ","model":"gpt-x!"}`),
	toolCase("mcp update_profile model only", "agent-a-mcp", "update_profile", `{"model":"m"}`),
	toolCase("mcp update_profile neither", "agent-a-mcp", "update_profile", `{}`),
	toolCase("mcp activity_start", "agent-a-mcp", "activity_start", `{}`),
	toolCase("mcp activity_stop", "agent-a-mcp", "activity_stop", `{}`),
}
