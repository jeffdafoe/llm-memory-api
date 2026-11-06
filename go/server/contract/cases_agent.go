package contract

import "strings"

// agentCases cover the /v1/agent routes against the agent fixture
// (fixture_agent.go); /agent/tick and /agent/rate-limit run against the
// virtual agents (cases_agent_tick.go).
var agentCases = concat(agentLoginCases, agentRotateCases, agentProfileCases, agentStatusCases,
	agentInstructionsCases, agentMiscCases, agentRoutingCases, agentMemorySyncCases, agentConversationSyncCases,
	agentTickCases, agentRateLimitCases)

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
	agentMemorySyncPath   = "/v1/agent/memory/sync"
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
	one("agent tick with GET", Step{Method: "GET", Path: "/v1/agent/tick", Auth: "agent-a-session"}),
}

// syncCase is one memory-sync request whose saves index in the background:
// the embedding calls may interleave, so they compare as a multiset.
func syncCase(name, auth, body string, known ...KnownDifference) Scenario {
	sc := postCase(name, agentMemorySyncPath, auth, body, known...)
	sc.OutboundMultiset = true
	return sc
}

// syncFile is one memory.files entry; the arguments are JSON texts.
func syncFile(name, content, mtime string) string {
	return `{"filename":` + name + `,"content":` + content + `,"mtime":` + mtime + `}`
}

func syncFiles(files ...string) string {
	return `{"memory":{"files":[` + strings.Join(files, ",") + `]}}`
}

// Against the memory notes of the agent fixture (applyMemorySyncFixture).
var agentMemorySyncCases = []Scenario{
	// Remote names first in listing order (future.md, dup.md from "dup",
	// same.md, local-newer.md, remote-newer.md, retired.md, notes.txt), then
	// local-only names in Object.keys order ("7" and "10" first). __proto__
	// vanishes; constructor compares as unchanged against
	// Object.prototype.constructor.
	syncCase("agent memory sync push pull unchanged", "agent-a-session", `{"memory":{"files":[`+strings.Join([]string{
		syncFile(`"remote-newer.md"`, `"stale local copy"`, `"2026-10-01T00:00:00Z"`),
		syncFile(`"same.md"`, `"Same on both sides."`, `"2026-10-02T10:00:00Z"`),
		syncFile(`"local-newer.md"`, `"---\nname: Local Newer Title\n---\nUpdated locally."`, `"2026-10-03T00:00:00.1234567Z"`),
		syncFile(`"new-note.md"`, `"# New Note\n\nWritten locally."`, `1759300000000`),
		syncFile(`"deleted.md"`, `"Back again."`, `"2026-10-05T00:00:00Z"`),
		syncFile(`"plain_note-file.md"`, `"no heading here"`, `"2026-10-05T00:00:00+02:00"`),
		syncFile(`"10"`, `"ten"`, `true`),
		syncFile(`"7"`, `"seven"`, `["2026-10-05T00:00:00Z"]`),
		syncFile(`"constructor"`, `"x"`, `"2026-10-05T00:00:00Z"`),
		syncFile(`"__proto__"`, `"x"`, `"2026-10-05T00:00:00Z"`),
	}, ",")+`]},"conversations":{}}`),
	{
		Name:             "agent memory sync prune",
		OutboundMultiset: true,
		Steps: []Step{
			// Everything remote-only and older than the scan is pruned;
			// future.md was written after it and pulls.
			{Method: "POST", Path: agentMemorySyncPath, Auth: "agent-a-key", Header: jsonType,
				Body: `{"memory":{"prune":true,"scan_age_ms":0,"files":[` + syncFile(`"same.md"`, `"Same on both sides."`, `"2026-10-02T10:00:00Z"`) + `]}}`},
			{Settle: true},
			// dup.md now comes from the older "dup.md" note, which the
			// collision hid.
			{Method: "POST", Path: agentMemorySyncPath, Auth: "agent-a-key", Header: jsonType, Body: `{"memory":{"files":[]}}`},
		},
	},
	syncCase("agent memory sync prune with the oldest scan age", "agent-a-session",
		`{"memory":{"prune":true,"scan_age_ms":3600000,"files":[]}}`),
	syncCase("agent memory sync prune flag not true", "agent-a-session",
		`{"memory":{"prune":"yes","scan_age_ms":"bad","files":[]}}`),
	syncCase("agent memory sync push over quota", "agent-b-session",
		syncFiles(syncFile(`"big.md"`, `"`+strings.Repeat("quota ", 40)+`"`, `"2026-10-05T00:00:00Z"`))),
	syncCase("agent memory sync push empty content", "agent-a-session",
		syncFiles(syncFile(`"empty.md"`, `""`, `"2026-10-05T00:00:00Z"`))),
	syncCase("agent memory sync push heading with no title", "agent-a-session",
		syncFiles(syncFile(`"blank.md"`, `"#   \n"`, `"2026-10-05T00:00:00Z"`))),
	syncCase("agent memory sync push a name that is not a slug", "agent-a-session",
		syncFiles(syncFile(`"tab\there.md"`, `"tab"`, `"2026-10-05T00:00:00Z"`))),
	postCase("agent memory sync files missing", agentMemorySyncPath, "agent-a-session", `{"memory":{"prune":true}}`),
	postCase("agent memory sync memory true", agentMemorySyncPath, "agent-a-session", `{"memory":true}`),
	postCase("agent memory sync file null", agentMemorySyncPath, "agent-a-session", syncFiles(`null`)),
	postCase("agent memory sync file array", agentMemorySyncPath, "agent-a-session", syncFiles(`[]`)),
	postCase("agent memory sync file name traversal", agentMemorySyncPath, "agent-a-session",
		syncFiles(syncFile(`"same.md"`, `""`, `"2026-10-05T00:00:00Z"`), syncFile(`"../x.md"`, `""`, `"2026-10-05T00:00:00Z"`))),
	postCase("agent memory sync file name a number", agentMemorySyncPath, "agent-a-session", syncFiles(syncFile(`5`, `""`, `1`))),
	postCase("agent memory sync content a number", agentMemorySyncPath, "agent-a-session", syncFiles(syncFile(`"a.md"`, `5`, `1`))),
	postCase("agent memory sync mtime missing", agentMemorySyncPath, "agent-a-session", syncFiles(`{"filename":"a.md","content":""}`)),
	postCase("agent memory sync mtime not a date", agentMemorySyncPath, "agent-a-session", syncFiles(syncFile(`"a.md"`, `""`, `"soon"`))),
	postCase("agent memory sync mtime past the Date range", agentMemorySyncPath, "agent-a-session", syncFiles(syncFile(`"a.md"`, `""`, `8640000000000001`))),
	postCase("agent memory sync mtime without a primitive", agentMemorySyncPath, "agent-a-session", syncFiles(syncFile(`"a.md"`, `""`, `{"toString":1}`))),
	// Node reads a free-form date; constructor then compares as unchanged,
	// and agent-b has no memory notes to pull, so Node writes nothing.
	postCase("agent memory sync mtime free-form", agentMemorySyncPath, "agent-b-session",
		syncFiles(syncFile(`"constructor"`, `"x"`, `"Oct 9 2026"`)),
		KnownDifference{Where: "step 1 POST " + agentMemorySyncPath + ": *", Reason: "a free-form mtime is a 400 in Go (go-server note, Known differences)"},
		KnownDifference{Where: "table request_log*", Reason: "the request log records the status and length the client got (go-server note, Known differences)"}),
	postCase("agent memory sync prune without a scan age", agentMemorySyncPath, "agent-a-session", `{"memory":{"prune":true,"files":[]}}`),
	postCase("agent memory sync prune scan age too old", agentMemorySyncPath, "agent-a-session", `{"memory":{"prune":true,"scan_age_ms":3600001,"files":[]}}`),
	postCase("agent memory sync prune scan age negative", agentMemorySyncPath, "agent-a-session", `{"memory":{"prune":true,"scan_age_ms":-1,"files":[]}}`),
	postCase("agent memory sync prune scan age a string", agentMemorySyncPath, "agent-a-session", `{"memory":{"prune":true,"scan_age_ms":"10","files":[]}}`),
	postCase("agent memory sync as an admin", agentMemorySyncPath, "admin-web", syncFiles()),
	postCase("agent memory sync nothing asked", agentMemorySyncPath, "agent-a-session", `{"memory":0,"conversations":null}`),
	postCase("agent memory sync array body", agentMemorySyncPath, "agent-a-session", `[{"memory":{"files":[]}}]`),
	agentNoBody(agentMemorySyncPath),
	one("agent memory sync encoded path", Step{Method: "POST", Path: "/v1/agent/memory/%73ync", Auth: "agent-a-session", Header: jsonType, Body: syncFiles()}),
	one("agent memory sync GET falls through", Step{Method: "GET", Path: agentMemorySyncPath, Auth: "agent-a-session"}),
}

// Session ids of agent-a's conversation logs in the fixture (fixture_agent.go) (memory-sync's diff).
const (
	syncSessionLive    = "11111111-1111-4111-8111-111111111111"
	syncSessionDeleted = "22222222-2222-4222-8222-222222222222"
	syncSessionOdd     = "33333333-3333-4333-8333-333333333333"
	syncSessionNew     = "44444444-4444-4444-8444-444444444444"
)

func syncSession(id, size string) string {
	return `{"id":"` + id + `","file_size":` + size + `}`
}

// Against the conversation logs of the agent fixture.
var agentConversationSyncCases = []Scenario{
	// The live session's largest size is its retired copy's 300; the
	// soft-deleted session is present; the odd one's stored size is not a
	// number, so 0.
	postCase("agent conversation sync sized sessions", agentMemorySyncPath, "agent-a-session",
		`{"conversations":{"sessions":[`+strings.Join([]string{
			syncSession(strings.ToUpper(syncSessionLive), `400`), syncSession(syncSessionDeleted, `10`),
			syncSession(syncSessionOdd, `1`), syncSession(syncSessionNew, `"5"`), syncSession(syncSessionLive, `999`),
			syncSession("not-a-uuid", `1`), `null`, `7`, `[1]`, `{"id":5}`, syncSession(syncSessionNew+"0", `1`),
		}, ",")+`],"session_ids":["`+syncSessionNew+`"]}}`),
	postCase("agent conversation sync sizes not grown", agentMemorySyncPath, "agent-a-session",
		`{"conversations":{"sessions":[`+syncSession(syncSessionLive, `300`)+`,`+syncSession(syncSessionOdd, `-4`)+`,`+
			syncSession(syncSessionDeleted, `1.5`)+`]}}`),
	postCase("agent conversation sync legacy ids", agentMemorySyncPath, "agent-b-session",
		`{"conversations":{"session_ids":["`+syncSessionLive+`","`+syncSessionNew+`",5,"bad"]}}`),
	postCase("agent conversation sync legacy ids of agent-a", agentMemorySyncPath, "agent-a-key",
		`{"conversations":{"session_ids":["`+syncSessionLive+`","`+syncSessionNew+`"],"sessions":[]}}`),
	postCase("agent conversation sync no valid ids", agentMemorySyncPath, "agent-a-session", `{"conversations":{"sessions":[{"id":"x"}]}}`),
	postCase("agent conversation sync retention only", agentMemorySyncPath, "agent-a-session", `{"conversations":5}`),
	postCase("agent conversation sync too many ids", agentMemorySyncPath, "agent-a-session",
		`{"conversations":{"session_ids":[`+strings.TrimSuffix(strings.Repeat(`"`+syncSessionNew+`",`, 1001), ",")+`],"uploads":[{"date":"d"}]}}`),
	syncCase("agent conversation sync uploads", "agent-a-session", `{"conversations":{"uploads":[`+strings.Join([]string{
		`{"content":"[11:00] agent-a: a new log","session_id":"` + syncSessionNew + `","date":"2026-10-06",` +
			`"metadata":{"session_id":"` + syncSessionNew + `","file_size":42,"project":"p"}}`,
		`{"content":"[10:00] agent-a: hello\n[10:05] agent-a: grown","session_id":"` + syncSessionLive + `","date":"2026-10-01",` +
			`"metadata":{"session_id":"` + syncSessionLive + `","file_size":150}}`,
		`{"session_id":5,"date":"d"}`,
		`"text"`,
		`{"content":"c","session_id":"s1","date":"2026//10"}`,
		`{"content":"c","session_id":"s2","date":"d","metadata":[1]}`,
		`{"content":"[12:00] agent-a: no metadata","session_id":"😀😀😀😀x","date":"2026-10-07","metadata":0}`,
	}, ",")+`]}}`),
	// Node failed in Buffer.byteLength with its own TypeError text.
	postCase("agent conversation sync upload content a number", agentMemorySyncPath, "agent-a-session",
		`{"conversations":{"uploads":[{"content":5,"session_id":"s","date":"d"}]}}`,
		KnownDifference{Where: "step 1 POST " + agentMemorySyncPath + ": body.conversations.upload_errors.0.error",
			Reason: "an upload field that is not a string is refused with Go's text (go-server note, Known differences)"},
		KnownDifference{Where: "table error_log row 1.error_message", Reason: "the same refusal, logged as conversation-upload (go-server note, Known differences)"},
		KnownDifference{Where: "table request_log row 1.response_length", Reason: "the request log records the length the client got (go-server note, Known differences)"}),
	syncCase("agent conversation sync null upload", "agent-a-session",
		`{"conversations":{"sessions":[`+syncSession(syncSessionNew, `1`)+`],"uploads":[{"content":"[09:00] agent-a: before the null","session_id":"`+syncSessionNew+`","date":"2026-10-08"},null]}}`),
	postCase("agent conversation sync file size without a primitive", agentMemorySyncPath, "agent-a-session",
		`{"memory":{"files":[`+syncFile(`"same.md"`, `"Same on both sides."`, `"2026-10-02T10:00:00Z"`)+`]},`+
			`"conversations":{"sessions":[{"id":"`+syncSessionLive+`","file_size":{"toString":1}}]}}`),
}
