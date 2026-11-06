package contract

import "strings"

const (
	agentTickPath      = "/v1/agent/tick"
	agentRateLimitPath = "/v1/agent/rate-limit"
)

// tickAs calls /agent/tick as a fixture virtual agent, with its API key.
func tickAs(va, body string) Step {
	return Step{Method: "POST", Path: agentTickPath, Auth: va + "-key", Header: jsonType, Body: body}
}

// tickTools is one neutral tool definition, which each provider adapter
// turns into its own shape in the request.
const tickTools = `[{"name":"speak","description":"Say something aloud","parameters":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}]`

// tickHistory is an OpenAI-shape conversation with a tool call and its
// result, as the salem engine sends it on its second call of a tick.
const tickHistory = `[{"role":"user","content":"You stand in the square. Anne Hale is here."},` +
	`{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"speak","arguments":"{\"text\":\"Good morning\"}"}}]},` +
	`{"role":"tool","tool_call_id":"call_1","content":"Anne Hale heard you."}]`

// anthropicToolUse is a reply with text and a tool call.
var anthropicToolUse = QueuedReply{Upstream: "anthropic", Path: "/v1/messages",
	Reply: JSONReply(0, `{"id":"msg_tick","type":"message","role":"assistant","model":"claude-haiku-4-5",`+
		`"content":[{"type":"text","text":"I greet her."},{"type":"tool_use","id":"toolu_1","name":"speak","input":{"text":"Good morning"}}],`+
		`"stop_reason":"tool_use","usage":{"input_tokens":120,"output_tokens":30}}`)}

// agentTickCases call /agent/tick as the fixture's virtual agents: a
// perception with tools on each provider, a whole conversation with a
// system prompt, the refusals and failures, and the request checks.
var agentTickCases = func() []Scenario {
	var out []Scenario
	for _, va := range fixtureVirtualAgents[:6] {
		sc := Scenario{
			Name:  "agent tick on " + va.provider,
			Steps: []Step{tickAs(va.name, `{"perception":"You stand in the square. Anne Hale is here.","tools":`+tickTools+`}`)},
			Known: []KnownDifference{callDuration},
		}
		if va.provider == "openrouter" {
			sc.Replies = []QueuedReply{openrouterCatalog}
		}
		out = append(out, sc)
	}
	return append(out,
		Scenario{
			Name:    "agent tick returns tool calls",
			Replies: []QueuedReply{anthropicToolUse},
			Steps:   []Step{tickAs("va-anthropic", `{"perception":"Anne Hale is here.","tools":`+tickTools+`}`)},
			Known:   []KnownDifference{callDuration},
		},
		Scenario{
			Name:  "agent tick with messages and a system prompt",
			Steps: []Step{tickAs("va-anthropic", `{"messages":`+tickHistory+`,"tools":`+tickTools+`,"system":"You are a villager of Salem."}`)},
			Known: []KnownDifference{callDuration},
		},
		Scenario{
			Name:    "agent tick with messages on openrouter",
			Replies: []QueuedReply{openrouterCatalog},
			Steps:   []Step{tickAs("va-openrouter", `{"messages":`+tickHistory+`,"tools":[],"system":""}`)},
			Known:   []KnownDifference{callDuration},
		},
		Scenario{
			Name: "agent tick rate-limited",
			Steps: []Step{
				tickAs("va-paced", `{"perception":"First tick."}`),
				tickAs("va-paced", `{"perception":"Second tick."}`),
			},
			Known: []KnownDifference{callDuration},
		},
		one("agent tick over the cost limit", tickAs("va-no-budget", `{"perception":"Tick."}`)),
		one("agent tick without an API key", tickAs("va-no-key", `{"perception":"Tick."}`)),
		Scenario{
			Name:    "agent tick provider failure",
			Replies: []QueuedReply{anthropicError},
			Steps:   []Step{tickAs("va-anthropic", `{"perception":"Tick."}`)},
			Known:   []KnownDifference{callDuration},
		},
		one("agent tick as an agent that is not virtual",
			Step{Method: "POST", Path: agentTickPath, Auth: "agent-a-key", Header: jsonType, Body: `{"perception":"Tick."}`}),
		one("agent tick as the admin", Step{Method: "POST", Path: agentTickPath, Auth: "admin-web", Header: jsonType, Body: `{"perception":"Tick."}`}),
		one("agent tick without a JSON body", Step{Method: "POST", Path: agentTickPath, Auth: "va-anthropic-key"}),
		Scenario{
			Name: "agent tick refuses bad bodies",
			Steps: []Step{
				tickAs("va-anthropic", `{}`),
				tickAs("va-anthropic", `{"perception":"","messages":[]}`),
				tickAs("va-anthropic", `{"perception":5}`),
				tickAs("va-anthropic", `["perception"]`),
				tickAs("va-anthropic", `{"perception":"x","messages":[{"role":"user","content":"y"}]}`),
				tickAs("va-anthropic", `{"perception":"x","tools":{"name":"speak"}}`),
				tickAs("va-anthropic", `{"perception":"x","tools":null}`),
				tickAs("va-anthropic", `{"perception":"x","system":null}`),
				tickAs("va-anthropic", `{"perception":"x","system":["s"]}`),
			},
		},
		one("agent tick encoded path", Step{Method: "POST", Path: "/v1/agent/%74ick", Auth: "va-anthropic-key", Header: jsonType, Body: `{"perception":"Tick."}`}),
	)
}()

// rateLimitAs calls /agent/rate-limit as agent-a.
func rateLimitAs(body string) Step {
	return Step{Method: "POST", Path: agentRateLimitPath, Auth: "agent-a-session", Header: jsonType, Body: body}
}

// agentRateLimitCases ask for the effective limits of a batch of slugs:
// the config defaults, an agent's own overrides (one a NaN), repeats, an
// array-index slug, __proto__ and an unknown name; then the refusals.
var agentRateLimitCases = []Scenario{
	one("agent rate-limit batch", rateLimitAs(`{"agents":["va-paced","va-anthropic","123","va-paced","__proto__","nobody","va-anthropic"]}`)),
	one("agent rate-limit as a virtual agent", Step{Method: "POST", Path: agentRateLimitPath, Auth: "va-paced-key", Header: jsonType, Body: `{"agents":["va-paced"]}`}),
	{
		Name: "agent rate-limit refuses bad batches",
		Steps: []Step{
			rateLimitAs(`{}`),
			rateLimitAs(`{"agents":[]}`),
			rateLimitAs(`{"agents":"va-paced"}`),
			rateLimitAs(`[["va-paced"]]`),
			rateLimitAs(`{"agents":["va-paced",7]}`),
			rateLimitAs(`{"agents":["va-paced",""]}`),
			rateLimitAs(`{"agents":["` + strings.Repeat("a", 201) + `"]}`),
			rateLimitAs(`{"agents":["` + strings.Repeat("\U0001F600", 100) + `x"]}`),
			rateLimitAs(`{"agents":[` + strings.TrimSuffix(strings.Repeat(`"a",`, 201), ",") + `]}`),
		},
	},
	one("agent rate-limit at the slug bound", rateLimitAs(`{"agents":["`+strings.Repeat("b", 200)+`","`+strings.Repeat("\U0001F600", 100)+`"]}`)),
	one("agent rate-limit as the admin", Step{Method: "POST", Path: agentRateLimitPath, Auth: "admin-web", Header: jsonType, Body: `{"agents":["va-paced"]}`}),
	one("agent rate-limit without a JSON body", Step{Method: "POST", Path: agentRateLimitPath, Auth: "agent-a-session"}),
}
