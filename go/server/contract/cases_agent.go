package contract

import "strings"

// agentCases cover the /v1/agent routes against the agent fixture
// (fixture_agent.go). /agent/tick and /agent/rate-limit are not ported yet
// (they call the virtual-agent service).
var agentCases = concat(agentLoginCases, agentRotateCases, agentProfileCases, agentStatusCases,
	agentInstructionsCases, agentMiscCases, agentRoutingCases)

const (
	agentLoginPath        = "/v1/agent/login"
	agentLogoutPath       = "/v1/agent/logout"
	agentRotatePath       = "/v1/agent/rotate"
	agentHeartbeatPath    = "/v1/agent/heartbeat"
	agentStatusPath       = "/v1/agent/status"
	agentExpertisePath    = "/v1/agent/expertise"
	agentProfilePath      = "/v1/agent/profile"
	agentActivityStart    = "/v1/agent/activity/start"
	agentActivityStop     = "/v1/agent/activity/stop"
	agentSignupEventPath  = "/v1/agent/signup-event"
	agentInstructionsRead = "/v1/agent/instructions/read"
	agentInstructionsSave = "/v1/agent/instructions/save"
	agentSyncMappingsPath = "/v1/agent/sync-mappings"
	agentConfigPath       = "/v1/agent/config"
)

// loginStep logs agent-a in and captures the session token as "token".
func loginStep(body string) Step {
	return Step{Method: "POST", Path: agentLoginPath, Header: jsonType, Body: body,
		Capture: map[string]string{"token": "session_token"}, Random: []string{"session_token"}}
}

// withToken is a step that authenticates with the captured login token.
func withToken(path, body string) Step {
	return Step{Method: "POST", Path: path, Header: [][2]string{{"Content-Type", "application/json"}, {"Authorization", "Bearer {{token}}"}}, Body: body}
}

// storedOnlyByNode is notAStringInAdminSession for a request Node completed:
// Node also wrote the value Go refused.
func storedOnlyByNode(label, table string) []KnownDifference {
	return append(notAStringInAdminSession(label),
		KnownDifference{Where: "table " + table + "*", Reason: "Node stored the value Go refused (go-server note, Known differences)"})
}

var agentLoginCases = []Scenario{
	{
		Name: "agent login, use the session, log out",
		Steps: []Step{
			loginStep(`{"agent":"agent-a","passphrase":"{{agent-a-passphrase}}","subsystem":" memory-sync! "}`),
			withToken(agentStatusPath, `{}`),
			withToken(agentLogoutPath, `{}`),
			withToken(agentStatusPath, `{}`),
		},
	},
	{
		Name: "agent login name sanitized, no subsystem",
		Steps: []Step{
			loginStep(`{"agent":" Agent-A ","passphrase":"{{agent-a-passphrase}}","subsystem":""}`),
			withToken(agentHeartbeatPath, `{}`),
		},
	},
	postCase("agent login wrong passphrase", agentLoginPath, "", `{"agent":"agent-a","passphrase":"wrong"}`),
	postCase("agent login unknown agent", agentLoginPath, "", `{"agent":"nobody-here","passphrase":"x"}`),
	postCase("agent login actor without a passphrase", agentLoginPath, "", `{"agent":"contract-admin","passphrase":"x"}`),
	postCase("agent login virtual agent", agentLoginPath, "", `{"agent":"helper-va","passphrase":"x"}`),
	postCase("agent login missing fields", agentLoginPath, "", `{"agent":"agent-a"}`),
	postCase("agent login name sanitized to nothing", agentLoginPath, "", `{"agent":"!!!","passphrase":"x"}`),
	postCase("agent login agent a number", agentLoginPath, "", `{"agent":5,"passphrase":"x"}`,
		notAStringInAdminSession("POST "+agentLoginPath)...),
	postCase("agent logout with an API key", agentLogoutPath, "agent-b-key", `{}`),
	postCase("agent logout as an admin", agentLogoutPath, "admin-web", `{}`),
	{
		Name: "agent logout ends the fixture session",
		Steps: []Step{
			{Method: "POST", Path: agentLogoutPath, Auth: "agent-b-session", Header: jsonType, Body: `{}`},
			{Method: "POST", Path: agentHeartbeatPath, Auth: "agent-b-session", Header: jsonType, Body: `{}`},
			{Method: "POST", Path: agentHeartbeatPath, Auth: "agent-b-key", Header: jsonType, Body: `{}`},
		},
	},
}

var agentRotateCases = []Scenario{
	{
		Name: "agent rotate",
		Steps: []Step{
			{Method: "POST", Path: agentRotatePath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"current_passphrase":"{{agent-a-passphrase}}"}`, Random: []string{"passphrase"}, VolatileLength: true},
			// Every API session ended; the API key still works.
			{Method: "POST", Path: agentHeartbeatPath, Auth: "agent-a-session", Header: jsonType, Body: `{}`},
			{Method: "POST", Path: agentHeartbeatPath, Auth: "agent-a-key", Header: jsonType, Body: `{}`},
			{Method: "POST", Path: agentLoginPath, Header: jsonType, Body: `{"agent":"agent-a","passphrase":"{{agent-a-passphrase}}"}`},
		},
	},
	postCase("agent rotate wrong passphrase", agentRotatePath, "agent-b-session", `{"current_passphrase":"wrong"}`),
	postCase("agent rotate missing passphrase", agentRotatePath, "agent-b-session", `{}`),
	// The admin has no passphrase salt: crypto.pbkdf2 threw in Node.
	postCase("agent rotate as an admin", agentRotatePath, "admin-web", `{"current_passphrase":"x"}`),
	postCase("agent rotate passphrase a number", agentRotatePath, "agent-b-session", `{"current_passphrase":5}`,
		notAStringInAdminSession("POST "+agentRotatePath)...),
}

var agentProfileCases = []Scenario{
	postCase("agent expertise", agentExpertisePath, "agent-b-session", `{"expertise":[" Go ",5,"","  ",null,["x"],"PostGres​?","<b>&"]}`),
	postCase("agent expertise empty", agentExpertisePath, "agent-a-session", `{"expertise":[]}`),
	postCase("agent expertise not an array", agentExpertisePath, "agent-a-session", `{"expertise":"go"}`),
	postCase("agent expertise as an admin", agentExpertisePath, "admin-web", `{"expertise":["go"]}`),
	postCase("agent profile both", agentProfilePath, "agent-a-session", `{"provider":" openai! ","model":"gpt-x.1"}`),
	postCase("agent profile model only", agentProfilePath, "agent-b-key", `{"model":"claude-y"}`),
	postCase("agent profile provider null", agentProfilePath, "agent-b-session", `{"provider":null}`),
	postCase("agent profile empty strings", agentProfilePath, "agent-b-session", `{"provider":"","model":"!!"}`),
	postCase("agent profile neither", agentProfilePath, "agent-a-session", `{"other":"x"}`),
	postCase("agent profile provider a number", agentProfilePath, "agent-a-session", `{"provider":5}`,
		storedOnlyByNode("POST "+agentProfilePath, "agent_configuration")...),
	postCase("agent activity start", agentActivityStart, "agent-a-session", `{}`),
	postCase("agent activity stop", agentActivityStop, "agent-b-session", `{}`),
	postCase("agent activity start as an admin", agentActivityStart, "admin-web", `{}`),
	postCase("agent heartbeat", agentHeartbeatPath, "agent-a-key", `{}`),
	postCase("agent heartbeat as an admin", agentHeartbeatPath, "admin-web", `{}`),
}

// statusAfterHeartbeat reads the roster after the caller's own heartbeat.
// Every /v1 request also stamps last_seen in the background, in both
// servers; whether that write lands before the roster query is a race in
// both, so the caller's status ("online" or "unknown") is fixed first.
func statusAfterHeartbeat(name, auth string) Scenario {
	return Scenario{Name: name, Steps: []Step{
		{Method: "POST", Path: agentHeartbeatPath, Auth: auth, Header: jsonType, Body: `{}`},
		{Method: "POST", Path: agentStatusPath, Auth: auth, Header: jsonType, Body: `{}`},
	}}
}

var agentStatusCases = []Scenario{
	// agent-a: itself, and agent-b by grant (helper-va's grant is outside
	// its realm). Its unread mail and chat are counted per sender.
	statusAfterHeartbeat("agent status as agent-a", "agent-a-session"),
	// agent-b: the wildcard row, inside its realm.
	statusAfterHeartbeat("agent status as agent-b with a key", "agent-b-key"),
	// The admin is superadmin: every agent, counts of 0. Its own row is not
	// an agent, so no heartbeat is needed.
	postCase("agent status as an admin", agentStatusPath, "admin-web", `{}`),
	{
		Name: "agent status shows new state",
		Steps: []Step{
			{Method: "POST", Path: agentHeartbeatPath, Auth: "agent-a-session", Header: jsonType, Body: `{}`},
			{Method: "POST", Path: agentActivityStart, Auth: "agent-b-session", Header: jsonType, Body: `{}`},
			{Method: "POST", Path: agentExpertisePath, Auth: "agent-b-session", Header: jsonType, Body: `{"expertise":["Watch"]}`},
			loginStep(`{"agent":"agent-b","passphrase":"{{agent-b-passphrase}}","subsystem":"zz-sub"}`),
			{Method: "POST", Path: agentStatusPath, Auth: "agent-a-session", Header: jsonType, Body: `{}`},
		},
	},
}

var agentInstructionsCases = []Scenario{
	postCase("agent instructions read", agentInstructionsRead, "agent-a-session", `{}`),
	// Dream bootstrap and the soul note; the read stamps the note's
	// last_accessed in the background.
	{
		Name: "agent instructions read with bootstrap and soul",
		Steps: []Step{
			{Method: "POST", Path: agentInstructionsRead, Auth: "agent-b-session", Header: jsonType, Body: `{}`},
			{Settle: true},
		},
	},
	postCase("agent instructions read as an admin", agentInstructionsRead, "admin-web", `{}`),
	{
		Name: "agent instructions save then read",
		Steps: []Step{
			{Method: "POST", Path: agentInstructionsSave, Auth: "agent-a-session", Header: jsonType, Body: `{"content":"New​ text 😀"}`},
			{Method: "POST", Path: agentInstructionsRead, Auth: "agent-a-session", Header: jsonType, Body: `{}`},
		},
	},
	{
		Name: "agent instructions save null then read",
		Steps: []Step{
			{Method: "POST", Path: agentInstructionsSave, Auth: "agent-a-key", Header: jsonType, Body: `{"content":null}`},
			{Method: "POST", Path: agentInstructionsRead, Auth: "agent-a-key", Header: jsonType, Body: `{}`},
		},
	},
	postCase("agent instructions save empty", agentInstructionsSave, "agent-a-session", `{"content":""}`),
	postCase("agent instructions save missing", agentInstructionsSave, "agent-a-session", `{"text":"x"}`),
	postCase("agent instructions save a number", agentInstructionsSave, "agent-a-session", `{"content":5}`,
		storedOnlyByNode("POST "+agentInstructionsSave, "agent_configuration")...),
}

var agentMiscCases = []Scenario{
	postCase("agent signup event", agentSignupEventPath, "agent-a-key", `{"event":"client","value":"claude-code"}`),
	postCase("agent signup event unknown event", agentSignupEventPath, "agent-a-key", `{"event":"other","value":"x"}`),
	postCase("agent signup event bad value", agentSignupEventPath, "agent-a-key", `{"event":"copy","value":"Bad Value"}`),
	postCase("agent signup event as an admin", agentSignupEventPath, "admin-web", `{"event":"copy","value":"x"}`),
	one("agent signup event without a body", Step{Method: "POST", Path: agentSignupEventPath, Auth: "agent-a-key"}),
	postCase("agent sync mappings", agentSyncMappingsPath, "agent-a-session", `{}`),
	postCase("agent sync mappings none", agentSyncMappingsPath, "agent-b-session", `{}`),
	postCase("agent sync mappings as an admin", agentSyncMappingsPath, "admin-web", `{}`),
	postCase("agent config", agentConfigPath, "agent-b-key", `{}`),
	postCase("agent config as an admin", agentConfigPath, "admin-web", `{}`),
}

// agentNoBody sends a body that is not JSON, so req.body is undefined in
// Node.
func agentNoBody(path string) Scenario {
	return one("agent "+strings.TrimPrefix(path, "/v1/agent/")+" without a JSON body", Step{Method: "POST", Path: path, Auth: "agent-a-session",
		Header: [][2]string{{"Content-Type", "text/plain"}}, Body: "x"})
}

var agentRoutingCases = []Scenario{
	one("agent login without a JSON body", Step{Method: "POST", Path: agentLoginPath, Header: [][2]string{{"Content-Type", "text/plain"}}, Body: "x"}),
	agentNoBody(agentRotatePath), agentNoBody(agentExpertisePath), agentNoBody(agentProfilePath), agentNoBody(agentInstructionsSave),
	agentNoBody(agentHeartbeatPath),
	one("agent status OPTIONS", Step{Method: "OPTIONS", Path: agentStatusPath, Auth: "agent-a-session"}),
	one("agent login OPTIONS", Step{Method: "OPTIONS", Path: agentLoginPath}),
	one("agent status GET falls through", Step{Method: "GET", Path: agentStatusPath, Auth: "agent-a-session"}),
	one("agent status encoded path", Step{Method: "POST", Path: "/v1/%61gent/status", Auth: "agent-a-session", Header: jsonType, Body: `{}`}),
	one("agent status without credentials", Step{Method: "POST", Path: agentStatusPath, Header: jsonType, Body: `{}`}),
	one("agent tick is not ported", Step{Method: "GET", Path: "/v1/agent/tick", Auth: "agent-a-session"}),
}
