package contract

// adminCases cover the admin router's mount (requireUser in front of every
// admin route), the account routes, config, providers and /v1/auth/verify.
var adminCases = concat(adminMountCases, adminLoginCases, adminLogoutCases, adminPasswordCases,
	adminLinkCases, adminProfileCases, adminConfigCases, adminProviderCases, authVerifyCases, adminActorCases, adminAgentCases, adminAccessCases, adminSightCases)

const (
	adminLoginPath          = "/v1/admin/login"
	adminLogoutPath         = "/v1/admin/logout"
	adminChangePasswordPath = "/v1/admin/change-password"
	adminLinkPath           = "/v1/admin/link-account"
	adminProfilePath        = "/v1/admin/profile/visibility"
	adminConfigListPath     = "/v1/admin/config/list"
	adminConfigUpdatePath   = "/v1/admin/config/update"
	adminRegistryPath       = "/v1/admin/providers/registry"
	adminDefaultsPath       = "/v1/admin/providers/defaults"
	adminOpenRouterPath     = "/v1/admin/providers/openrouter/models"
	authVerifyPath          = "/v1/auth/verify"
)

// noBodyPost is a POST with no body: Express leaves req.body undefined.
func noBodyPost(name, path, auth string, known ...KnownDifference) Scenario {
	return one(name, Step{Method: "POST", Path: path, Auth: auth}, known...)
}

// notAStringDirect is the known difference for a refused non-string field
// on a route whose errors Node never logged (its own try/catch, or a direct
// res.status().json()): the answer and the request-log row differ, nothing
// else.
func notAStringDirect(label string) []KnownDifference {
	return []KnownDifference{
		{Where: "step 1 " + label + ": *", Reason: "a request-body field of a JSON type the route cannot use is a 400 in Go (go-server note, Known differences)"},
		{Where: "table request_log*", Reason: "the request log records the status and length the client got (go-server note, Known differences)"},
	}
}

// adminLoginStep logs in with the dashboard password and captures the web
// session token as "token".
func adminLoginStep(body string) Step {
	return Step{Method: "POST", Path: adminLoginPath, Header: jsonType, Body: body,
		Capture: map[string]string{"token": "session_token"}, Random: []string{"session_token"}}
}

var adminMountCases = []Scenario{
	// The session middleware runs before the admin router: no token is its
	// 401, an agent token reaches requireUser's 401.
	one("admin OPTIONS without auth", Step{Method: "OPTIONS", Path: adminConfigListPath}),
	one("admin OPTIONS as an agent", Step{Method: "OPTIONS", Path: adminConfigListPath, Auth: "agent-a-session"}),
	one("admin OPTIONS as an admin", Step{Method: "OPTIONS", Path: adminConfigListPath, Auth: "admin-web"}),
	one("admin GET as an admin", Step{Method: "GET", Path: adminConfigListPath, Auth: "admin-web"}),
	one("admin GET as an agent", Step{Method: "GET", Path: adminConfigListPath, Auth: "agent-a-key"}),
	postCase("admin POST as an agent", adminConfigListPath, "agent-a-session", `{}`),
	postCase("admin encoded path as an admin", "/v1/admin/config%2Flist", "admin-web", `{}`),
	postCase("admin encoded login path without auth", "/v1/admin%2Flogin", "", `{}`),
	postCase("admin unknown path as an admin", "/v1/admin/no-such-route", "admin-web", `{}`),
	one("admin login OPTIONS without auth", Step{Method: "OPTIONS", Path: adminLoginPath}),
}

var adminLoginCases = []Scenario{
	{
		Name: "admin login, use the session, log out",
		Steps: []Step{
			adminLoginStep(`{"username":" Contract-Admin ","password":"{{admin-password}}"}`),
			withToken(adminConfigListPath, `{}`),
			withToken(adminLogoutPath, `{}`),
			withToken(adminConfigListPath, `{}`),
		},
	},
	{
		Name: "admin login as the limited admin",
		Steps: []Step{
			adminLoginStep(`{"username":"contract-admin-limited","password":"{{admin-limited-password}}"}`),
			withToken(adminConfigListPath, `{}`),
		},
	},
	postCase("admin login wrong password", adminLoginPath, "", `{"username":"contract-admin","password":"wrong"}`),
	postCase("admin login unknown name", adminLoginPath, "", `{"username":"nobody-here","password":"x"}`),
	postCase("admin login account without a password", adminLoginPath, "", `{"username":"agent-b","password":"x"}`),
	postCase("admin login missing password", adminLoginPath, "", `{"username":"contract-admin"}`),
	postCase("admin login name sanitized away", adminLoginPath, "", `{"username":"!!!","password":"x"}`),
	postCase("admin login array body", adminLoginPath, "", `["username"]`),
	noBodyPost("admin login no body", adminLoginPath, ""),
	postCase("admin login number name", adminLoginPath, "", `{"username":5,"password":"x"}`,
		notAStringDirect("POST "+adminLoginPath)...),
	postCase("admin login object password", adminLoginPath, "", `{"username":"contract-admin","password":{"a":1}}`,
		notAStringDirect("POST "+adminLoginPath)...),
}

var adminLogoutCases = []Scenario{
	{
		Name: "admin logout ends the fixture session",
		Steps: []Step{
			{Method: "POST", Path: adminLogoutPath, Auth: "admin-web"},
			{Method: "POST", Path: adminConfigListPath, Auth: "admin-web", Header: jsonType, Body: `{}`},
		},
	},
	postCase("admin logout as an agent", adminLogoutPath, "agent-a-session", `{}`),
}

var adminPasswordCases = []Scenario{
	{
		Name: "admin change password, then log in with it",
		Steps: []Step{
			{Method: "POST", Path: adminChangePasswordPath, Auth: "admin-web", Header: jsonType,
				Body: `{"current_password":"{{admin-password}}","new_password":"a much longer password"}`},
			{Method: "POST", Path: adminLoginPath, Header: jsonType, Body: `{"username":"contract-admin","password":"{{admin-password}}"}`},
			adminLoginStep(`{"username":"contract-admin","password":"a much longer password"}`),
		},
	},
	postCase("admin change password wrong current", adminChangePasswordPath, "admin-web",
		`{"current_password":"wrong","new_password":"a much longer password"}`),
	postCase("admin change password too short", adminChangePasswordPath, "admin-web",
		`{"current_password":"{{admin-password}}","new_password":"short"}`),
	postCase("admin change password missing", adminChangePasswordPath, "admin-web", `{"current_password":"x"}`),
	noBodyPost("admin change password no body", adminChangePasswordPath, "admin-web"),
	postCase("admin change password number", adminChangePasswordPath, "admin-web",
		`{"current_password":"{{admin-password}}","new_password":123456789012}`,
		notAStringDirect("POST "+adminChangePasswordPath)...),
}

func linkStep(auth, body string) Step {
	return Step{Method: "POST", Path: adminLinkPath, Auth: auth, Header: jsonType, Body: body}
}

var adminLinkCases = []Scenario{
	{
		Name: "admin link account twice",
		Steps: []Step{
			linkStep("admin-web", `{"username":" Agent-A ","password":"`+AgentAPassword+`"}`),
			linkStep("admin-web", `{"username":"agent-a","password":"`+AgentAPassword+`"}`),
		},
	},
	postCase("admin link account wrong password", adminLinkPath, "admin-web", `{"username":"agent-a","password":"wrong"}`),
	postCase("admin link account unknown", adminLinkPath, "admin-web", `{"username":"nobody-here","password":"x"}`),
	postCase("admin link account to oneself", adminLinkPath, "admin-web", `{"username":"contract-admin","password":"{{admin-password}}"}`),
	postCase("admin link account no shared realm", adminLinkPath, "admin-limited-web", `{"username":"agent-a","password":"`+AgentAPassword+`"}`),
	postCase("admin link account missing", adminLinkPath, "admin-web", `{"username":"agent-a"}`),
	noBodyPost("admin link account no body", adminLinkPath, "admin-web"),
	{
		Name: "admin link account throttled after five attempts",
		Steps: []Step{
			linkStep("admin-web", `{"username":"agent-a","password":"w1"}`),
			linkStep("admin-web", `{"username":"agent-a","password":"w2"}`),
			linkStep("admin-web", `{"username":"agent-b","password":"w3"}`),
			linkStep("admin-web", `{"username":"agent-b","password":"w4"}`),
			linkStep("admin-web", `{"username":"nobody-here","password":"w5"}`),
			linkStep("admin-web", `{"username":"agent-a","password":"`+AgentAPassword+`"}`),
		},
	},
}

var adminProfileCases = []Scenario{
	{
		Name: "admin profile visibility set and read",
		Steps: []Step{
			{Method: "POST", Path: adminProfilePath, Auth: "admin-web", Header: jsonType, Body: `{}`},
			{Method: "POST", Path: adminProfilePath, Auth: "admin-web", Header: jsonType, Body: `{"visible_to_others":"yes"}`},
			{Method: "POST", Path: adminProfilePath, Auth: "admin-web", Header: jsonType, Body: `{}`},
			{Method: "POST", Path: adminProfilePath, Auth: "admin-web", Header: jsonType, Body: `{"visible_to_others":null}`},
		},
	},
	// Node checks no permission on this route.
	postCase("admin profile visibility as the limited admin", adminProfilePath, "admin-limited-web", `{"visible_to_others":true}`),
	postCase("admin profile visibility unknown actor", adminProfilePath, "admin-web", `{"actor_id":999999}`),
	postCase("admin profile visibility bad actor id", adminProfilePath, "admin-web", `{"actor_id":"abc"}`),
	noBodyPost("admin profile visibility no body", adminProfilePath, "admin-web"),
}

var adminConfigCases = []Scenario{
	postCase("admin config list", adminConfigListPath, "admin-web", `{}`),
	postCase("admin config list limited", adminConfigListPath, "admin-limited-web", `{}`),
	{
		Name: "admin config update takes effect at once",
		Steps: []Step{
			{Method: "GET", Path: "/api/registration-mode"},
			{Method: "POST", Path: adminConfigUpdatePath, Auth: "admin-web", Header: jsonType, Body: `{"key":"open_registration","value":"true"}`},
			{Method: "GET", Path: "/api/registration-mode"},
		},
	},
	postCase("admin config update empties a value", adminConfigUpdatePath, "admin-web", `{"key":"sync_exclude_slugs","value":0}`),
	postCase("admin config update unknown key", adminConfigUpdatePath, "admin-web", `{"key":"no_such_key","value":"x"}`),
	postCase("admin config update missing key", adminConfigUpdatePath, "admin-web", `{"value":"x"}`),
	postCase("admin config update limited", adminConfigUpdatePath, "admin-limited-web", `{"key":"open_registration","value":"true"}`),
	noBodyPost("admin config update no body", adminConfigUpdatePath, "admin-web"),
	postCase("admin config update number value", adminConfigUpdatePath, "admin-web", `{"key":"open_registration","value":5}`,
		storedOnlyByNode("POST "+adminConfigUpdatePath, "config")...),
}

var adminProviderCases = []Scenario{
	postCase("admin providers registry", adminRegistryPath, "admin-limited-web", `{}`),
	postCase("admin providers defaults", adminDefaultsPath, "admin-web", `{"provider":"anthropic","model":"claude-haiku-4-5"}`),
	postCase("admin providers defaults unknown model", adminDefaultsPath, "admin-web", `{"provider":"anthropic","model":"no-such-model"}`),
	postCase("admin providers defaults missing", adminDefaultsPath, "admin-web", `{"provider":"anthropic"}`),
	noBodyPost("admin providers defaults no body", adminDefaultsPath, "admin-web"),
	postCase("admin openrouter models empty catalog", adminOpenRouterPath, "admin-web", `{}`),
	{
		Name:    "admin openrouter models",
		Steps:   []Step{{Method: "POST", Path: adminOpenRouterPath, Auth: "admin-web", Header: jsonType, Body: `{}`}},
		Replies: []QueuedReply{openrouterCatalog},
	},
}

var authVerifyCases = []Scenario{
	postCase("auth verify web session", authVerifyPath, "", `{"token":"{{admin-web}}"}`),
	postCase("auth verify limited web session", authVerifyPath, "", `{"token":"{{admin-limited-web}}"}`),
	postCase("auth verify api session", authVerifyPath, "", `{"token":"{{agent-a-session}}"}`),
	postCase("auth verify api key is not a session", authVerifyPath, "", `{"token":"{{agent-a-key}}"}`),
	postCase("auth verify expired session", authVerifyPath, "", `{"token":"{{agent-b-expired-session}}"}`),
	postCase("auth verify missing token", authVerifyPath, "", `{}`),
	noBodyPost("auth verify no body", authVerifyPath, ""),
	postCase("auth verify number token", authVerifyPath, "", `{"token":5}`,
		KnownDifference{Where: "step 1 POST " + authVerifyPath + ": *", Reason: "a request-body field of a JSON type the route cannot use is a 400 in Go (go-server note, Known differences)"},
		KnownDifference{Where: "table request_log*", Reason: "the request log records the status and length the client got (go-server note, Known differences)"}),
	one("auth verify OPTIONS", Step{Method: "OPTIONS", Path: authVerifyPath}),
	one("auth verify GET goes on to auth", Step{Method: "GET", Path: authVerifyPath}),
	one("auth verify GET as an admin", Step{Method: "GET", Path: authVerifyPath, Auth: "admin-web"}),
	postCase("auth verify encoded path", "/v1/auth%2Fverify", "", `{"token":"x"}`),
}
