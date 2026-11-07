package contract

// adminAgentCases cover /admin/agents* and /admin/actors/create.
var adminAgentCases = concat(adminAgentListCases, adminAgentInstructionCases, adminAgentExpertiseCases,
	adminAgentResetCases, adminAgentReadCases, adminAgentUpdateCases, adminAgentUsageCases, adminActorCreateCases)

const (
	agentsListPath         = "/v1/admin/agents"
	agentsInstructionsRead = "/v1/admin/agents/instructions/read"
	agentsInstructionsSave = "/v1/admin/agents/instructions/save"
	agentsExpertiseSave    = "/v1/admin/agents/expertise/save"
	agentsResetPassphrase  = "/v1/admin/agents/reset-passphrase"
	agentsReadPath         = "/v1/admin/agents/read"
	agentsUpdatePath       = "/v1/admin/agents/update"
	agentsUsagePath        = "/v1/admin/agents/usage"
	agentsCallDetailPath   = "/v1/admin/agents/call-detail"
	actorsCreatePath       = "/v1/admin/actors/create"
)

// grantLimitedSight lets the limited admin see agent-a without owning it,
// before its first request (no visibility cache holds the old set).
var grantLimitedSight = Step{SQL: `INSERT INTO actor_visibility_configuration (actor_id, target_actor_id)
    SELECT l.id, a.id FROM actors l, actors a WHERE l.name = 'contract-admin-limited' AND a.name = 'agent-a'`}

// vaUsage gives va-anthropic two usage rows, one today and one ten days
// ago, each with its call row.
var vaUsage = Step{SQL: `
    INSERT INTO virtual_agent_usage (id, provider, model, input_tokens, output_tokens, cost, context, created_at, actor_id, status)
    SELECT 9001, 'anthropic', 'claude-x', 10, 20, 0.012345, 'mail', NOW(), id, 'success' FROM actors WHERE name = 'va-anthropic';
    INSERT INTO virtual_agent_usage (id, provider, model, input_tokens, output_tokens, cost, context, created_at, actor_id, status, error_message)
    SELECT 9002, 'anthropic', 'claude-x', 1, 0, 1.5, 'chat', NOW() - INTERVAL '10 days', id, 'error', 'boom' FROM actors WHERE name = 'va-anthropic';
    INSERT INTO virtual_agent_calls (id, actor_id, context, context_id, provider, model, system_prompt, user_message, response, status, status_code, cost, duration_ms, created_at, usage_id, sim_actor_id, sim_actor_name)
    SELECT 9101, id, 'mail', 'm-1', 'anthropic', 'claude-x', 'sys', 'hello', 'hi', 'success', 200, 0.012345, 321, NOW(), 9001, 'npc-1', 'Ann' FROM actors WHERE name = 'va-anthropic';
    INSERT INTO virtual_agent_calls (id, actor_id, provider, model, status, created_at, usage_id)
    SELECT 9102, id, 'anthropic', 'claude-x', 'error', NOW() - INTERVAL '10 days', 9002 FROM actors WHERE name = 'va-anthropic';`}

// welcomeTemplates adds a welcome template (9001), a welcome-note template
// (9002) and a welcome template whose body is only its header (9003).
var welcomeTemplates = Step{SQL: `
    INSERT INTO templates (id, name, kind, content) VALUES
      (9001, 'contract welcome', 'welcome', E'---\nsubject: Hello {agent}\n---\n\nWelcome, {agent}. Read your notes.\n'),
      (9002, 'contract note', 'welcome-note', E'---\ntitle: Start here, {agent}\nslug: instructions/start\n---\n\n# Start\n\nFor {agent}.\n'),
      (9003, 'contract empty', 'welcome', E'---\nsubject: Empty\n---\n\n');`}

var adminAgentListCases = []Scenario{
	postCase("admin agents list", agentsListPath, "admin-web", `{}`),
	postCase("admin agents list limited", agentsListPath, "admin-limited-web", `{}`),
	noBodyPost("admin agents list no body", agentsListPath, "admin-web"),
	{
		Name:    "admin agents list with an OpenRouter agent",
		Steps:   []Step{adminPost(agentsListPath, "admin-web", `{}`)},
		Replies: []QueuedReply{openrouterCatalog},
	},
	withIDs("admin agents list summaries and config",
		Step{SQL: `UPDATE config SET value = ' a , ,b ' WHERE key = 'hosted_realms';
                   UPDATE config SET value = 'lots' WHERE key = 'default_storage_quota';
                   INSERT INTO virtual_agent_access (virtual_agent_id, grantee_actor_id) SELECT id, NULL FROM actors WHERE name = 'va-google';`},
		adminPost(agentsListPath, "admin-limited-web", `{}`),
		adminPost(agentsListPath, "admin-web", `{}`)),
}

var adminAgentInstructionCases = []Scenario{
	postCase("admin agents instructions read", agentsInstructionsRead, "admin-web", `{"agent":" Agent-A "}`),
	postCase("admin agents instructions read none set", agentsInstructionsRead, "admin-web", `{"agent":"agent-b"}`),
	postCase("admin agents instructions read no configuration", agentsInstructionsRead, "admin-web", `{"agent":"contract-admin"}`),
	postCase("admin agents instructions read hidden", agentsInstructionsRead, "admin-limited-web", `{"agent":"agent-a"}`),
	postCase("admin agents instructions read unknown", agentsInstructionsRead, "admin-web", `{"agent":"nobody"}`),
	postCase("admin agents instructions read missing agent", agentsInstructionsRead, "admin-web", `{"agent":"   "}`),
	noBodyPost("admin agents instructions read no body", agentsInstructionsRead, "admin-web"),
	postCase("admin agents instructions read number agent", agentsInstructionsRead, "admin-web", `{"agent":5}`,
		notAStringInAdminSession("POST "+agentsInstructionsRead)...),
	withIDs("admin agents instructions save",
		adminPost(agentsInstructionsSave, "admin-web", `{"agent":"agent-b","content":"Be brief. 😀"}`),
		adminPost(agentsInstructionsRead, "admin-web", `{"agent":"agent-b"}`),
		adminPost(agentsInstructionsSave, "admin-limited-web", `{"agent":"limited-agent","content":""}`),
		adminPost(agentsInstructionsRead, "admin-limited-web", `{"agent":"limited-agent"}`)),
	withIDs("admin agents instructions save refusals",
		grantLimitedSight,
		adminPost(agentsInstructionsSave, "admin-web", `{"agent":"agent-b"}`),
		adminPost(agentsInstructionsSave, "admin-limited-web", `{"agent":"agent-a","content":"x"}`),
		adminPost(agentsInstructionsSave, "admin-limited-web", `{"agent":"agent-b","content":"x"}`),
		adminPost(agentsInstructionsSave, "admin-web", `{"agent":"contract-admin","content":"x"}`)),
	// Node stores the NULL and then fails reading its length.
	withIDs("admin agents instructions save null",
		adminPost(agentsInstructionsSave, "admin-web", `{"agent":"agent-a","content":null}`),
		adminPost(agentsInstructionsRead, "admin-web", `{"agent":"agent-a"}`)),
	postCase("admin agents instructions save number content", agentsInstructionsSave, "admin-web", `{"agent":"agent-a","content":5}`,
		storedAsText("POST "+agentsInstructionsSave, "startup_instructions")...),
	// Visibility and ownership come before the content's type, as in Node.
	withIDs("admin agents instructions save number content for another's agent",
		grantLimitedSight,
		adminPost(agentsInstructionsSave, "admin-limited-web", `{"agent":"agent-a","content":5}`),
		adminPost(agentsInstructionsSave, "admin-limited-web", `{"agent":"agent-b","content":5}`)),
	noBodyPost("admin agents instructions save no body", agentsInstructionsSave, "admin-web"),
}

var adminAgentExpertiseCases = []Scenario{
	withIDs("admin agents expertise save",
		adminPost(agentsExpertiseSave, "admin-web", `{"agent":"agent-a","expertise":[" Go ","","ÇA",7,null,"  ","Rust"]}`),
		adminPost(agentsReadPath, "admin-web", `{"agent":"agent-a"}`),
		adminPost(agentsExpertiseSave, "admin-limited-web", `{"agent":"limited-agent","expertise":[]}`)),
	withIDs("admin agents expertise save refusals",
		grantLimitedSight,
		adminPost(agentsExpertiseSave, "admin-web", `{"agent":"agent-a","expertise":"go"}`),
		adminPost(agentsExpertiseSave, "admin-web", `{"expertise":["go"]}`),
		adminPost(agentsExpertiseSave, "admin-limited-web", `{"agent":"agent-a","expertise":["go"]}`),
		adminPost(agentsExpertiseSave, "admin-web", `{"agent":"nobody","expertise":["go"]}`)),
	noBodyPost("admin agents expertise save no body", agentsExpertiseSave, "admin-web"),
}

var adminAgentResetCases = []Scenario{
	{
		// The old API session is used first, so both servers hold it in
		// their auth cache; the reset must drop it there too. The API key
		// stays valid.
		Name: "admin agents reset passphrase",
		Steps: []Step{
			adminPost("/v1/agent/heartbeat", "agent-b-session", `{}`),
			{Method: "POST", Path: agentsResetPassphrase, Auth: "admin-web", Header: jsonType, Body: `{"agent":" Agent-B "}`,
				Capture: map[string]string{"phrase": "passphrase"}, Random: []string{"passphrase"}, VolatileLength: true},
			adminPost("/v1/agent/heartbeat", "agent-b-session", `{}`),
			adminPost("/v1/agent/heartbeat", "agent-b-key", `{}`),
			adminPost(agentLoginPath, "", `{"agent":"agent-b","passphrase":"{{agent-b-passphrase}}"}`),
			loginStep(`{"agent":"agent-b","passphrase":"{{phrase}}"}`),
			withToken("/v1/agent/heartbeat", `{}`),
		},
		Known: []KnownDifference{{Where: "table request_log row 6.request_length", Volatile: true,
			Reason: "the login body carries the new passphrase, whose length is random"}},
	},
	withIDs("admin agents reset passphrase refusals",
		grantLimitedSight,
		adminPost(agentsResetPassphrase, "admin-limited-web", `{"agent":"agent-a"}`),
		adminPost(agentsResetPassphrase, "admin-limited-web", `{"agent":"agent-b"}`),
		adminPost(agentsResetPassphrase, "admin-web", `{"agent":""}`),
		adminPost("/v1/agent/heartbeat", "agent-a-session", `{}`)),
	noBodyPost("admin agents reset passphrase no body", agentsResetPassphrase, "admin-web"),
}

var adminAgentReadCases = []Scenario{
	postCase("admin agents read", agentsReadPath, "admin-web", `{"agent":"agent-b"}`),
	postCase("admin agents read virtual", agentsReadPath, "admin-web", `{"agent":"va-paced"}`),
	postCase("admin agents read limited", agentsReadPath, "admin-limited-web", `{"agent":"limited-va"}`),
	postCase("admin agents read hidden", agentsReadPath, "admin-limited-web", `{"agent":"agent-a"}`),
	postCase("admin agents read no configuration", agentsReadPath, "admin-web", `{"agent":"contract-admin"}`),
	noBodyPost("admin agents read no body", agentsReadPath, "admin-web"),
	{
		Name:    "admin agents read OpenRouter pricing",
		Steps:   []Step{adminPost(agentsReadPath, "admin-web", `{"agent":"va-openrouter"}`)},
		Replies: []QueuedReply{openrouterCatalog},
	},
	withIDs("admin agents read costs and limits",
		vaUsage,
		Step{SQL: `UPDATE config SET value = 'none' WHERE key = 'virtual_agent_default_monthly_budget';
                   UPDATE config SET value = '2.5' WHERE key = 'virtual_agent_default_daily_budget';
                   UPDATE agent_configuration SET cost_budget_monthly = 40 WHERE actor_id = (SELECT id FROM actors WHERE name = 'va-openai');`},
		adminPost(agentsReadPath, "admin-web", `{"agent":"va-anthropic"}`),
		adminPost(agentsReadPath, "admin-web", `{"agent":"va-openai"}`)),
}

var adminAgentUpdateCases = []Scenario{
	withIDs("admin agents update",
		adminPost(agentsUpdatePath, "admin-web", `{"agent":"va-anthropic","personality":"Calm.","configuration":{"temperature":"","top_p":0.9,"note":""},`+
			`"provider":"openai","model":"gpt-x","cost_budget_daily":"1.25","cost_budget_monthly":null,"cache_prompts":true,"learning_enabled":false,`+
			`"max_tokens":"512","temperature":"0.4","storage_quota":0,"dream_mode":"sim-shared","dream_source":"notes","realms":["contract","r2"]}`),
		adminPost(agentsReadPath, "admin-web", `{"agent":"va-anthropic"}`),
		adminPost(agentsUpdatePath, "admin-web", `{"agent":"va-anthropic","personality":0,"configuration":["a"],"provider":"","max_tokens":"","temperature":null,"storage_quota":null,"cost_budget_daily":""}`),
		adminPost(agentsReadPath, "admin-web", `{"agent":"va-anthropic"}`),
		adminPost(agentsUpdatePath, "admin-web", `{"agent":"va-anthropic","temperature":"warm","realms":[1,null,["x"]]}`),
		adminPost(agentsReadPath, "admin-web", `{"agent":"va-anthropic"}`)),
	withIDs("admin agents update realms only",
		adminPost(agentsUpdatePath, "admin-limited-web", `{"agent":"limited-agent","realms":[]}`),
		adminPost(agentsReadPath, "admin-limited-web", `{"agent":"limited-agent"}`)),
	{
		// The encrypted key has a random IV, so the stored text differs by
		// design; has_api_key and the cleared key are compared.
		Name: "admin agents update api key",
		Steps: []Step{
			adminPost(agentsUpdatePath, "admin-web", `{"agent":"va-no-key","api_key":"sk-contract"}`),
			adminPost(agentsReadPath, "admin-web", `{"agent":"va-no-key"}`),
			adminPost(agentsUpdatePath, "admin-web", `{"agent":"va-google","api_key":""}`),
			adminPost(agentsReadPath, "admin-web", `{"agent":"va-google"}`),
		},
		Known: []KnownDifference{{Where: "table agent_configuration row *.api_key", Volatile: true,
			Reason: "provider.js encrypts with a random IV, so the stored ciphertext differs between any two runs"}},
	},
	withIDs("admin agents update refusals",
		grantLimitedSight,
		adminPost(agentsUpdatePath, "admin-web", `{"agent":"va-anthropic"}`),
		adminPost(agentsUpdatePath, "admin-web", `{"agent":"va-anthropic","realms":"r"}`),
		adminPost(agentsUpdatePath, "admin-web", `{"agent":"va-anthropic","max_tokens":0}`),
		adminPost(agentsUpdatePath, "admin-web", `{"agent":"va-anthropic","max_tokens":"1.5"}`),
		adminPost(agentsUpdatePath, "admin-web", `{"agent":"va-anthropic","storage_quota":"-1"}`),
		adminPost(agentsUpdatePath, "admin-web", `{"agent":"va-anthropic","dream_mode":"deep"}`),
		adminPost(agentsUpdatePath, "admin-web", `{"agent":"va-anthropic","dream_source":null}`),
		adminPost(agentsUpdatePath, "admin-web", `{"agent":"va-anthropic","cost_budget_daily":"abc"}`),
		adminPost(agentsUpdatePath, "admin-web", `{"agent":"va-anthropic","cost_budget_monthly":100000000}`),
		adminPost(agentsUpdatePath, "admin-web", `{"agent":"contract-admin","model":"m"}`),
		adminPost(agentsUpdatePath, "admin-limited-web", `{"agent":"agent-a","model":"m"}`),
		adminPost(agentsUpdatePath, "admin-limited-web", `{"agent":"agent-b","model":"m"}`),
		adminPost(agentsReadPath, "admin-web", `{"agent":"va-anthropic"}`)),
	postCase("admin agents update number model", agentsUpdatePath, "admin-web", `{"agent":"va-anthropic","model":5}`,
		storedAsText("POST "+agentsUpdatePath, "model")...),
	noBodyPost("admin agents update no body", agentsUpdatePath, "admin-web"),
}

var adminAgentUsageCases = []Scenario{
	withIDs("admin agents usage and call detail",
		vaUsage,
		Step{Method: "POST", Path: agentsUsagePath, Auth: "admin-web", Header: jsonType, Body: `{"agent":"va-anthropic"}`,
			Capture: map[string]string{"call": "usage.0.call_id"}},
		adminPost(agentsUsagePath, "admin-web", `{"agent":"va-anthropic","limit":"1"}`),
		adminPost(agentsUsagePath, "admin-web", `{"agent":"va-anthropic","limit":0}`),
		adminPost(agentsUsagePath, "admin-web", `{"agent":"va-anthropic","limit":5000}`),
		adminPost(agentsUsagePath, "admin-web", `{"agent":"va-anthropic","limit":"many"}`),
		adminPost(agentsCallDetailPath, "admin-web", `{"call_id":{{call}}}`),
		adminPost(agentsCallDetailPath, "admin-web", `{"call_id":"9102"}`),
		// The limited admin cannot see va-anthropic, so its call is a 404.
		adminPost(agentsCallDetailPath, "admin-limited-web", `{"call_id":9101}`),
		adminPost(agentsUsagePath, "admin-limited-web", `{"agent":"va-anthropic"}`)),
	postCase("admin agents usage none", agentsUsagePath, "admin-limited-web", `{"agent":"limited-va"}`),
	postCase("admin agents usage missing agent", agentsUsagePath, "admin-web", `{"limit":5}`),
	noBodyPost("admin agents usage no body", agentsUsagePath, "admin-web"),
	postCase("admin agents call detail unknown", agentsCallDetailPath, "admin-web", `{"call_id":999999}`),
	postCase("admin agents call detail invalid", agentsCallDetailPath, "admin-web", `{"call_id":-4}`),
	noBodyPost("admin agents call detail no body", agentsCallDetailPath, "admin-web"),
}

// storedAsText is the known difference for a non-string field Node wrote
// to an agent_configuration text column, which Go refuses.
func storedAsText(label, column string) []KnownDifference {
	return append(notAStringInAdminSession(label), KnownDifference{Where: "table agent_configuration row *." + column,
		Reason: "Node stored the number as text; Go refuses it (go-server note, Known differences)"})
}

// createStep creates an actor; its passphrase is random.
func createStep(auth, body string) Step {
	return Step{Method: "POST", Path: actorsCreatePath, Auth: auth, Header: jsonType, Body: body,
		Random: []string{"passphrase"}, VolatileLength: true}
}

var adminActorCreateCases = []Scenario{
	withIDs("admin actors create with welcome templates",
		welcomeTemplates,
		createStep("admin-web", `{"name":"  New-Agent ","provider":"anthropic","model":"claude-x","personality":"Kind.",`+
			`"welcome_template_id":9001,"welcome_note_template_id":"9002","cost_budget_daily":"3","cost_budget_monthly":"","max_tokens":"256",`+
			`"temperature":"1.5","configuration":{"top_p":"","x":1},"dream_mode":"companion","dream_source":"notes","cache_prompts":true,"learning_enabled":0}`),
		adminPost(agentsReadPath, "admin-web", `{"agent":"new-agent"}`),
		adminPost(agentsInstructionsRead, "admin-web", `{"agent":"new-agent"}`),
		Step{Settle: true}),
	withIDs("admin actors create virtual",
		welcomeTemplates,
		createStep("admin-web", `{"name":"new-va","virtual":true,"provider":"anthropic","model":"claude-x","welcome_template_id":9001,"welcome_note_template_id":9002,"dream_mode":"dreamy"}`),
		createStep("admin-web", `{"name":"new-vb","virtual":"yes","welcome_template_id":9001}`),
		adminPost(agentsListPath, "admin-web", `{}`)),
	withIDs("admin actors create with dashboard access",
		createStep("admin-web", `{"name":"new-user","ui_access":"on","password":"long enough pw","email":"  person@example.com "}`),
		adminLoginStep(`{"username":"new-user","password":"long enough pw"}`),
		createStep("admin-web", `{"name":"no-ui","password":"ignored"}`)),
	withIDs("admin actors create as the limited admin",
		createStep("admin-limited-web", `{"name":"limited-new","configuration":"plain text"}`),
		adminPost(actorsListPath, "admin-limited-web", `{}`),
		adminPost(agentsReadPath, "admin-limited-web", `{"agent":"limited-new"}`)),
	withIDs("admin actors create template problems",
		welcomeTemplates,
		createStep("admin-web", `{"name":"tpl-a","welcome_template_id":9002,"welcome_note_template_id":9001}`),
		createStep("admin-web", `{"name":"tpl-b","welcome_template_id":"abc"}`),
		createStep("admin-web", `{"name":"tpl-c","welcome_template_id":9003,"welcome_note_template_id":"x"}`),
		createStep("admin-web", `{"name":"tpl-$&-d","welcome_template_id":9001}`),
		Step{Settle: true}),
	withIDs("admin actors create refusals",
		adminPost(actorsCreatePath, "admin-web", `{"name":"   "}`),
		adminPost(actorsCreatePath, "admin-web", `{"name":"Agent-A"}`),
		adminPost(actorsCreatePath, "admin-web", `{"name":"shared"}`),
		adminPost(actorsCreatePath, "admin-web", `{"name":"x1","ui_access":true}`),
		adminPost(actorsCreatePath, "admin-web", `{"name":"x1","ui_access":true,"password":"short"}`),
		adminPost(actorsCreatePath, "admin-web", `{"name":"x1","max_tokens":0}`),
		adminPost(actorsCreatePath, "admin-web", `{"name":"x1","temperature":"hot"}`),
		adminPost(actorsCreatePath, "admin-web", `{"name":"x1","temperature":3}`),
		adminPost(actorsCreatePath, "admin-web", `{"name":"x1","email":"`+longEmail+`"}`),
		adminPost(actorsCreatePath, "admin-web", `{"name":"x1","cost_budget_daily":-1}`),
		adminPost(actorsCreatePath, "admin-web", `{"name":"x1","cost_budget_monthly":"lots"}`),
		createStep("admin-web", `{"name":"x2"}`)),
	postCase("admin actors create as an agent", actorsCreatePath, "agent-a-session", `{"name":"x"}`),
	noBodyPost("admin actors create no body", actorsCreatePath, "admin-web"),
	postCase("admin actors create number name", actorsCreatePath, "admin-web", `{"name":5}`,
		notAStringInAdminSession("POST "+actorsCreatePath)...),
	// A taken name and a refused budget come before a text field's type,
	// as in Node, which never converted those fields.
	withIDs("admin actors create number provider with other refusals",
		adminPost(actorsCreatePath, "admin-web", `{"name":"agent-a","provider":5}`),
		adminPost(actorsCreatePath, "admin-web", `{"name":"x3","personality":[1],"cost_budget_daily":-1}`),
		createStep("admin-web", `{"name":"x4"}`)),
}

var longEmail = func() string {
	s := ""
	for i := 0; i < 251; i++ {
		s += "e"
	}
	return s + "@x.io"
}()
