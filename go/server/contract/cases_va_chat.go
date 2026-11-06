package contract

import (
	"context"
	"fmt"
)

// engineName is the salem engine's actor (fixture_va_chat.go). A chat from
// it is a sim turn: the sim prompts, the scene's co-located people as
// Impressions, the per-scene rate limit, no retry on a wait=true tick.
const engineName = "salem-engine"

// vaChatted waits until the virtual agent has sent `to` n chat messages
// (replies and notices) and its activity spinner is off (it goes off 3 s
// after the call started, past a quiet settle). The last settle then waits
// for the call log. With replied, one of them must be a model's reply, not
// an [Error] or [Retrying] notice, so a case cannot pass by failing the same
// way on both sides.
func vaChatted(name, to string, n int, replied bool) Step {
	return Step{Settle: true, Until: func(ctx context.Context, t *Target) (bool, error) {
		return t.Holds(ctx, `WITH sent AS (
                SELECT cmt.message, cmt.is_error FROM chat_message_texts cmt
                JOIN chat_messages cm ON cm.message_text_id = cmt.id
                JOIN actors f ON f.id = cmt.from_actor_id JOIN actors r ON r.id = cm.to_actor_id
                WHERE f.name = $1 AND r.name = $2)
            SELECT (SELECT count(*) FROM sent) >= $3
                AND (NOT $4 OR EXISTS (SELECT 1 FROM sent WHERE NOT is_error))
                AND NOT EXISTS (SELECT 1 FROM actors WHERE name = $1 AND active_since IS NOT NULL)`,
			name, to, n, replied)
	}}
}

// chatVA is the admin's chat to a virtual agent on vaUser's behalf (the
// fixture's agents may use none of them); extra is more body fields.
func chatVA(name, message string, wait bool, extra string) Step {
	body := fmt.Sprintf(`{"from_agent":%q,"to_agents":[%q],"message":%q`, vaUser, name, message)
	if wait {
		body += `,"wait":true`
	}
	return Step{Method: "POST", Path: chatSendPath, Auth: "admin-web", Header: jsonType, Body: body + extra + "}"}
}

// engineChat is a sim turn: the admin sends as salem-engine to va-openrouter.
func engineChat(fields string) Step {
	return Step{Method: "POST", Path: chatSendPath, Auth: "admin-web", Header: jsonType,
		Body: `{"from_agent":"` + engineName + `","to_agents":["va-openrouter"],` + fields + `}`}
}

const (
	simScene = "6d2a0c1e-0000-4000-8000-0000000000e9"
	simTools = `[{"name":"speak","description":"Say something aloud.","parameters":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}},` +
		`{"name":"done","description":"End your turn.","parameters":{"type":"object","properties":{}}}]`
	simScene1 = `"scene_id":"` + simScene + `","scene_structure":"Inn","conversation_id":"conv-7","sim_actor_id":"npc-3","sim_actor_name":"Elias Ward","history_scope":"scene"`
)

// openRouterToolCall is a chat completion with two tool calls: the
// follow-up's history then has a parallel turn to split.
var openRouterToolCall = QueuedReply{Upstream: "openrouter", Path: "/api/v1/chat/completions", Reply: JSONReply(200,
	`{"id":"chatcmpl_tool","object":"chat.completion","model":"fake-model","choices":[{"index":0,"message":{"role":"assistant","content":"",`+
		`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"speak","arguments":"{\"text\":\"Good day, Anne.\"}"}},`+
		`{"id":"call_2","type":"function","function":{"name":"done","arguments":"{}"}}]},"finish_reason":"tool_calls"}],`+
		`"usage":{"prompt_tokens":40,"completion_tokens":12}}`)}

// vaChatCases chat with the virtual agents vaUser may use, and with
// va-openrouter as the salem engine: the reply (inline for wait=true), the
// chat rows (the reply acked on insert for wait=true, the inbound message
// acked by the agent), the usage and call rows and the provider request.
var vaChatCases = func() []Scenario {
	var out []Scenario
	for _, va := range fixtureVirtualAgents[:6] {
		sc := Scenario{
			Name:  "chat wait to a virtual agent on " + va.provider,
			Steps: []Step{chatVA(va.name, "What is the build status today?", true, ""), vaChatted(va.name, vaUser, 1, true)},
			Known: []KnownDifference{callDuration},
		}
		if va.provider == "openrouter" {
			sc.Replies = []QueuedReply{openrouterCatalog}
		}
		out = append(out, sc)
	}
	return append(out,
		Scenario{
			Name:  "chat to a virtual agent without wait",
			Steps: []Step{chatVA("va-anthropic", "Hello there.", false, ""), vaChatted("va-anthropic", vaUser, 1, true)},
			Known: []KnownDifference{callDuration},
		},
		Scenario{
			// The second turn replays the first as history.
			Name: "chat twice to a virtual agent",
			Steps: []Step{
				chatVA("va-anthropic", "What is the build status today?", true, ""), vaChatted("va-anthropic", vaUser, 1, true),
				chatVA("va-anthropic", "And tomorrow?", true, ""), vaChatted("va-anthropic", vaUser, 2, true),
			},
			Known: []KnownDifference{callDuration},
		},
		Scenario{
			Name:    "chat to a virtual agent that fails once",
			Replies: []QueuedReply{anthropicError},
			Steps:   []Step{chatVA("va-anthropic", "Hello there.", false, ""), vaChatted("va-anthropic", vaUser, 2, true)},
			Known:   []KnownDifference{callDuration},
		},
		Scenario{
			Name:    "chat wait to a virtual agent that keeps failing",
			Replies: []QueuedReply{anthropicError, anthropicError},
			Steps:   []Step{chatVA("va-anthropic", "Hello there.", true, ""), vaChatted("va-anthropic", vaUser, 2, false)},
			Known:   []KnownDifference{callDuration},
		},
		Scenario{
			Name:  "chat wait to a virtual agent with no API key",
			Steps: []Step{chatVA("va-no-key", "Hello there.", true, ""), {Settle: true}},
		},
		Scenario{
			Name:  "chat to a virtual agent with no API key without wait",
			Steps: []Step{chatVA("va-no-key", "Hello there.", false, ""), vaChatted("va-no-key", vaUser, 1, false)},
		},
		Scenario{
			Name:  "chat wait to a virtual agent over its budget",
			Steps: []Step{chatVA("va-no-budget", "Hello there.", true, ""), vaChatted("va-no-budget", vaUser, 2, false)},
		},
		Scenario{
			// A fresh perception with tools, the stable and ephemeral
			// context, then the tool results: the follow-up replays the
			// parallel calls split, each with its result and paraphrase,
			// and keeps Anne Hale's impressions from the scene cache.
			Name:    "sim turn with tool calls and their results",
			Replies: []QueuedReply{openrouterCatalog, openRouterToolCall},
			Steps: []Step{
				engineChat(`"message":"You stand in the Inn. Anne Hale comes in.","tools_offered":` + simTools + `,` + simScene1 +
					`,"stable_context":"You are Elias Ward, the innkeeper.","ephemeral_context":"## Around you\nhuddle: with Anne Hale, the smith\n\nWhat do you do?","wait":true`),
				vaChatted("va-openrouter", engineName, 1, true),
				engineChat(`"tool_call_results":[{"id":"call_1","content":"[OK] You spoke."},{"id":"call_2","content":"[done]"}],"tools_offered":` + simTools + `,` + simScene1 + `,"wait":true`),
				vaChatted("va-openrouter", engineName, 2, true),
			},
			Known: []KnownDifference{callDuration},
		},
		Scenario{
			// No tools: a prose reflection, with the sim reflection prompt.
			Name:    "sim reflection",
			Replies: []QueuedReply{openrouterCatalog},
			Steps:   []Step{engineChat(`"message":"Reflect on the day.\nhuddle: with Anne Hale","history_scope":"scene","wait":true`), vaChatted("va-openrouter", engineName, 1, true)},
			Known:   []KnownDifference{callDuration},
		},
		Scenario{
			// Not a wait=true tick, so it runs under the retry, in the background.
			Name:    "sim turn without wait",
			Replies: []QueuedReply{openrouterCatalog},
			Steps:   []Step{engineChat(`"message":"Morning in the Inn.","tools_offered":` + simTools + `,` + simScene1), vaChatted("va-openrouter", engineName, 1, true)},
			Known:   []KnownDifference{callDuration},
		},
	)
}()
