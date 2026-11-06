package contract

import (
	"context"
	"fmt"
)

// vaUser is the actor the virtual agents in fixtureVirtualAgents answer: a
// plain actor with no session, mailed from by the admin on its behalf.
const vaUser = "va-user"

// fixtureVirtualAgent is one virtual agent vaUser may use (fixture_va.go).
type fixtureVirtualAgent struct {
	name, provider, model string
	configVersion         int
	noKey                 bool
	// dailyBudget nil is the config default.
	dailyBudget *string
	// extraConf is more members of the configuration JSON, after
	// _configVersion.
	extraConf string
}

var zeroBudget = "0"

var fixtureVirtualAgents = []fixtureVirtualAgent{
	{name: "va-anthropic", provider: "anthropic", model: "claude-haiku-4-5", configVersion: 1},
	{name: "va-openai", provider: "openai", model: "gpt-6-luna", configVersion: 3},
	{name: "va-google", provider: "google", model: "gemini-2.5-flash", configVersion: 1},
	{name: "va-openrouter", provider: "openrouter", model: "deepseek/deepseek-v4-flash"},
	{name: "va-perplexity", provider: "perplexity", model: "sonar", configVersion: 1},
	{name: "va-xai", provider: "xai", model: "grok-4.1-fast-reasoning", configVersion: 1},
	{name: "va-no-key", provider: "anthropic", model: "claude-haiku-4-5", configVersion: 1, noKey: true},
	{name: "va-no-budget", provider: "anthropic", model: "claude-haiku-4-5", configVersion: 1, dailyBudget: &zeroBudget},
	// One call per 90 s window; the cooldown is a setting parseInt cannot
	// read (NaN), which /agent/rate-limit reports as null.
	{name: "va-paced", provider: "anthropic", model: "claude-haiku-4-5", configVersion: 1,
		extraConf: `,"rate_limit":1,"rate_window_seconds":"90","cooldown_seconds":"soon"`},
}

// vaMailed waits until the virtual agent has sent vaUser n mails and its
// activity spinner is off (it goes off 3 s after the call started, past a
// quiet settle). The last settle then waits for the call log. With replied,
// one of the mails must be a model's reply, not a notice ([Error],
// [Retrying], Delivery failed), so a case cannot pass by failing the same
// way on both sides.
func vaMailed(name string, n int, replied bool) Step {
	return Step{Settle: true, Until: func(ctx context.Context, t *Target) (bool, error) {
		return t.Holds(ctx, `SELECT
                (SELECT count(*) FROM mail m JOIN actors f ON f.id = m.from_actor_id JOIN actors r ON r.id = m.to_actor_id
                 WHERE f.name = $1 AND r.name = $2) >= $3
                AND (NOT $4 OR EXISTS (SELECT 1 FROM mail m JOIN actors f ON f.id = m.from_actor_id
                     WHERE f.name = $1 AND m.subject <> 'Delivery failed' AND m.body NOT LIKE '[%'))
                AND NOT EXISTS (SELECT 1 FROM actors WHERE name = $1 AND active_since IS NOT NULL)`,
			name, vaUser, n, replied)
	}}
}

func mailVA(name, subject string) Step {
	return Step{Method: "POST", Path: mailSendPath, Auth: "admin-web", Header: jsonType,
		Body: fmt.Sprintf(`{"to_agent":%q,"from_agent":%q,"subject":%q,"body":"What is the build status today?"}`, name, vaUser, subject)}
}

// callDuration is how long the provider call took, which differs between
// two correct runs.
var callDuration = KnownDifference{
	Where: "table virtual_agent_calls row *.duration_ms", Reason: "measured call time", Volatile: true,
}

// openrouterCatalog prices the fixture's OpenRouter model (per token, as
// OpenRouter's /api/v1/models lists it).
var openrouterCatalog = QueuedReply{Upstream: "openrouter", Path: "/api/v1/models",
	Reply: JSONReply(0, `{"data":[{"id":"deepseek/deepseek-v4-flash","name":"DeepSeek V4 Flash","context_length":131072,"pricing":{"prompt":"0.0000002","completion":"0.0000008"}}]}`)}

// anthropicError is a provider failure the agent retries.
var anthropicError = QueuedReply{Upstream: "anthropic", Path: "/v1/messages",
	Reply: JSONReply(500, `{"type":"error","error":{"type":"api_error","message":"Overloaded"}}`)}

// vaMailCases mail the virtual agents vaUser may use: the reply (threaded,
// the mail acked), the usage and call rows and the provider request for each
// provider, then the retry, failure and refusal paths. The fixture's retry
// backoff is 1 s, one retry.
var vaMailCases = func() []Scenario {
	var out []Scenario
	for _, va := range fixtureVirtualAgents[:6] {
		sc := Scenario{
			Name:  "mail to a virtual agent on " + va.provider,
			Steps: []Step{mailVA(va.name, "Question"), vaMailed(va.name, 1, true)},
			Known: []KnownDifference{callDuration},
		}
		if va.provider == "openrouter" {
			// The fake's default catalog is empty, which leaves the model
			// with no price and the call failing on its cost.
			sc.Replies = []QueuedReply{openrouterCatalog}
		}
		out = append(out, sc)
	}
	return append(out,
		Scenario{
			Name:  "mail to a virtual agent with a Re: subject",
			Steps: []Step{mailVA("va-anthropic", "Re: Status"), vaMailed("va-anthropic", 1, true)},
			Known: []KnownDifference{callDuration},
		},
		Scenario{
			Name:    "mail to a virtual agent that fails once",
			Replies: []QueuedReply{anthropicError},
			Steps:   []Step{mailVA("va-anthropic", "Question"), vaMailed("va-anthropic", 2, true)},
			Known:   []KnownDifference{callDuration},
		},
		Scenario{
			Name:    "mail to a virtual agent that keeps failing",
			Replies: []QueuedReply{anthropicError, anthropicError},
			Steps:   []Step{mailVA("va-anthropic", "Question"), vaMailed("va-anthropic", 2, false)},
			Known:   []KnownDifference{callDuration},
		},
		Scenario{
			Name:  "mail to a virtual agent with no API key",
			Steps: []Step{mailVA("va-no-key", "Question"), vaMailed("va-no-key", 1, false)},
		},
		Scenario{
			Name:  "mail to a virtual agent over its budget",
			Steps: []Step{mailVA("va-no-budget", "Question"), vaMailed("va-no-budget", 1, false)},
		},
	)
}()
