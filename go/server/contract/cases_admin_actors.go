package contract

// adminActorCases cover /admin/actors/* and /admin/virtual-agent-access/*.
// Actor ids are captured, not written: limitedIDs reads the limited
// admin's own list (itself, limited-agent, limited-va, in name order), and
// verifyID reads one actor's id from /auth/verify.
var adminActorCases = concat(adminActorListCases, adminActorPermissionCases, adminActorVisibilityCases,
	adminActorPasswordCases, adminActorAdminPermissionCases, adminActorDeleteCases, adminActorSearchCases, adminVAAccessCases)

const (
	actorsListPath           = "/v1/admin/actors/list"
	actorsPermissionsRead    = "/v1/admin/actors/permissions/read"
	actorsPermissionsSave    = "/v1/admin/actors/permissions/save"
	actorsVisibilityRead     = "/v1/admin/actors/visibility/read"
	actorsVisibilitySave     = "/v1/admin/actors/visibility/save"
	actorsPasswordPath       = "/v1/admin/actors/password"
	actorsNamespacesPath     = "/v1/admin/actors/namespaces"
	actorsAdminPermsRead     = "/v1/admin/actors/admin-permissions/read"
	actorsAdminPermsSave     = "/v1/admin/actors/admin-permissions/save"
	actorsDeletePath         = "/v1/admin/actors/delete"
	actorsSearchablePath     = "/v1/admin/actors/searchable"
	vaAccessListPath         = "/v1/admin/virtual-agent-access/list"
	vaAccessGrantPath        = "/v1/admin/virtual-agent-access/grant"
	vaAccessRevokePath       = "/v1/admin/virtual-agent-access/revoke"
	documentsListPathForPerm = "/v1/documents/list"
)

// limitedIDs captures limited_id, limited_agent_id and limited_va_id.
func limitedIDs() Step {
	return Step{Method: "POST", Path: actorsListPath, Auth: "admin-limited-web", Header: jsonType, Body: `{}`,
		Capture: map[string]string{"limited_id": "actors.0.id", "limited_agent_id": "actors.1.id", "limited_va_id": "actors.2.id"}}
}

// verifyID captures the actor id of a session credential as name.
func verifyID(name, credential string) Step {
	return Step{Method: "POST", Path: authVerifyPath, Header: jsonType, Body: `{"token":"{{` + credential + `}}"}`,
		Capture: map[string]string{name: "actor_id"}}
}

func adminPost(path, auth, body string) Step {
	return Step{Method: "POST", Path: path, Auth: auth, Header: jsonType, Body: body}
}

func withIDs(name string, steps ...Step) Scenario {
	return Scenario{Name: name, Steps: steps}
}

var adminActorListCases = []Scenario{
	postCase("admin actors list", actorsListPath, "admin-web", `{}`),
	postCase("admin actors list limited", actorsListPath, "admin-limited-web", `{}`),
	noBodyPost("admin actors list no body", actorsListPath, "admin-web"),
	postCase("admin actors list as an agent", actorsListPath, "agent-a-session", `{}`),
	postCase("admin actors namespaces", actorsNamespacesPath, "admin-web", `{}`),
	postCase("admin actors namespaces limited", actorsNamespacesPath, "admin-limited-web", `{}`),
	{
		Name:    "admin actors list with an OpenRouter agent",
		Steps:   []Step{adminPost(actorsListPath, "admin-web", `{}`)},
		Replies: []QueuedReply{openrouterCatalog},
	},
}

var adminActorPermissionCases = []Scenario{
	withIDs("admin actors permissions read",
		verifyID("a", "agent-a-session"),
		adminPost(actorsPermissionsRead, "admin-web", `{"actor_id":{{a}}}`),
		adminPost(actorsPermissionsRead, "admin-limited-web", `{"actor_id":"{{a}}"}`)),
	postCase("admin actors permissions read invalid id", actorsPermissionsRead, "admin-web", `{"actor_id":"abc"}`),
	postCase("admin actors permissions read zero", actorsPermissionsRead, "admin-web", `{"actor_id":0}`),
	postCase("admin actors permissions read unknown", actorsPermissionsRead, "admin-web", `{"actor_id":999999}`),
	noBodyPost("admin actors permissions read no body", actorsPermissionsRead, "admin-web"),
	postCase("admin actors permissions save needs superadmin", actorsPermissionsSave, "admin-limited-web", `{"actor_id":1,"permissions":[]}`),
	noBodyPost("admin actors permissions save no body", actorsPermissionsSave, "admin-web"),
	postCase("admin actors permissions save not an array", actorsPermissionsSave, "admin-web", `{"actor_id":1,"permissions":{}}`),
	withIDs("admin actors permissions save replaces and takes effect",
		verifyID("b", "agent-b-session"),
		adminPost(documentsListPathForPerm, "agent-b-session", `{"namespace":"shared"}`),
		adminPost(actorsPermissionsSave, "admin-web",
			`{"actor_id":{{b}},"permissions":[{"namespace":" agent-a ","can_read":1,"can_write":"","can_delete":null},{"namespace":"/","can_read":false}]}`),
		adminPost(actorsPermissionsRead, "admin-web", `{"actor_id":{{b}}}`),
		adminPost(documentsListPathForPerm, "agent-b-session", `{"namespace":"shared"}`)),
	withIDs("admin actors permissions save refusals",
		verifyID("b", "agent-b-session"),
		adminPost(actorsPermissionsSave, "admin-web", `{"actor_id":{{b}},"permissions":[{"namespace":"*"}]}`),
		adminPost(actorsPermissionsSave, "admin-web", `{"actor_id":{{b}},"permissions":[{"namespace":"  "}]}`),
		adminPost(actorsPermissionsSave, "admin-web", `{"actor_id":{{b}},"permissions":["shared"]}`),
		adminPost(actorsPermissionsSave, "admin-web", `{"actor_id":{{b}},"permissions":[{"namespace":"x"},{"namespace":" x"}]}`),
		adminPost(actorsPermissionsSave, "admin-web", `{"actor_id":{{b}},"permissions":[{"namespace":"x"},null]}`),
		adminPost(actorsPermissionsSave, "admin-web", `{"actor_id":999999,"permissions":[]}`),
		adminPost(actorsPermissionsRead, "admin-web", `{"actor_id":{{b}}}`)),
}

var adminActorVisibilityCases = []Scenario{
	withIDs("admin actors visibility read",
		verifyID("a", "agent-a-session"),
		verifyID("b", "agent-b-session"),
		adminPost(actorsVisibilityRead, "admin-web", `{"actor_id":{{a}}}`),
		adminPost(actorsVisibilityRead, "admin-web", `{"actor_id":{{b}}}`)),
	postCase("admin actors visibility read unknown", actorsVisibilityRead, "admin-web", `{"actor_id":999999}`),
	noBodyPost("admin actors visibility read no body", actorsVisibilityRead, "admin-web"),
	withIDs("admin actors visibility save replaces and takes effect",
		verifyID("a", "agent-a-session"),
		verifyID("b", "agent-b-session"),
		adminPost("/v1/agent/heartbeat", "agent-a-session", `{}`),
		adminPost("/v1/agent/status", "agent-a-session", `{}`),
		adminPost(actorsVisibilitySave, "admin-web", `{"actor_id":{{a}},"wildcard":false,"grants":["{{b}}",{{b}},{{a}}]}`),
		adminPost(actorsVisibilityRead, "admin-web", `{"actor_id":{{a}}}`),
		adminPost(actorsVisibilitySave, "admin-web", `{"actor_id":{{a}},"wildcard":false,"grants":[]}`),
		adminPost("/v1/agent/status", "agent-a-session", `{}`),
		adminPost(actorsVisibilitySave, "admin-web", `{"actor_id":{{a}},"wildcard":true,"grants":[]}`),
		adminPost(actorsVisibilityRead, "admin-web", `{"actor_id":{{a}}}`)),
	withIDs("admin actors visibility save refusals",
		verifyID("a", "agent-a-session"),
		adminPost(actorsVisibilitySave, "admin-web", `{"actor_id":{{a}},"wildcard":"yes","grants":[]}`),
		adminPost(actorsVisibilitySave, "admin-web", `{"actor_id":{{a}},"wildcard":false}`),
		adminPost(actorsVisibilitySave, "admin-web", `{"actor_id":{{a}},"wildcard":false,"grants":["x"]}`),
		adminPost(actorsVisibilitySave, "admin-web", `{"actor_id":{{a}},"wildcard":false,"grants":[999999]}`),
		adminPost(actorsVisibilitySave, "admin-web", `{"actor_id":999999,"wildcard":false,"grants":[]}`)),
	withIDs("admin actors visibility save ownership and the delegation bound",
		verifyID("a", "agent-a-session"),
		limitedIDs(),
		adminPost(actorsVisibilitySave, "admin-limited-web", `{"actor_id":{{a}},"wildcard":false,"grants":[]}`),
		adminPost(actorsVisibilitySave, "admin-limited-web", `{"actor_id":{{limited_agent_id}},"wildcard":false,"grants":[{{a}}]}`),
		adminPost(actorsVisibilitySave, "admin-limited-web", `{"actor_id":{{limited_agent_id}},"wildcard":true,"grants":[]}`),
		adminPost(actorsVisibilitySave, "admin-limited-web", `{"actor_id":{{limited_agent_id}},"wildcard":false,"grants":[{{limited_va_id}}]}`),
		adminPost(actorsVisibilitySave, "admin-limited-web", `{"actor_id":{{limited_id}},"wildcard":false,"grants":[{{limited_agent_id}}]}`),
		adminPost(actorsVisibilityRead, "admin-limited-web", `{"actor_id":{{limited_agent_id}}}`)),
}

var adminActorPasswordCases = []Scenario{
	withIDs("admin actors password set, log in, clear",
		limitedIDs(),
		adminPost(actorsPasswordPath, "admin-limited-web", `{"actor_id":{{limited_agent_id}},"password":"limited agent password"}`),
		adminPost(adminLoginPath, "", `{"username":"limited-agent","password":"limited agent password"}`),
		adminPost(actorsPasswordPath, "admin-limited-web", `{"actor_id":"{{limited_agent_id}}","password":null}`),
		adminPost(adminLoginPath, "", `{"username":"limited-agent","password":"limited agent password"}`)),
	withIDs("admin actors password refusals",
		verifyID("a", "agent-a-session"),
		limitedIDs(),
		adminPost(actorsPasswordPath, "admin-limited-web", `{"actor_id":{{a}},"password":"long enough password"}`),
		adminPost(actorsPasswordPath, "admin-limited-web", `{"actor_id":{{limited_agent_id}},"password":"short"}`),
		adminPost(actorsPasswordPath, "admin-limited-web", `{"actor_id":{{limited_agent_id}},"password":12345678901}`),
		adminPost(actorsPasswordPath, "admin-web", `{"actor_id":999999,"password":"long enough password"}`),
		adminPost(actorsPasswordPath, "admin-web", `{"actor_id":-1}`)),
	noBodyPost("admin actors password no body", actorsPasswordPath, "admin-web"),
}

var adminActorAdminPermissionCases = []Scenario{
	withIDs("admin actors admin permissions read",
		limitedIDs(),
		adminPost(actorsAdminPermsRead, "admin-web", `{"actor_id":{{limited_id}}}`),
		adminPost(actorsAdminPermsRead, "admin-limited-web", `{"actor_id":"{{limited_id}}"}`)),
	postCase("admin actors admin permissions read invalid", actorsAdminPermsRead, "admin-web", `{"actor_id":"1.5"}`),
	noBodyPost("admin actors admin permissions read no body", actorsAdminPermsRead, "admin-web"),
	postCase("admin actors admin permissions save needs superadmin", actorsAdminPermsSave, "admin-limited-web", `{"actor_id":1,"permissions":[]}`),
	noBodyPost("admin actors admin permissions save no body", actorsAdminPermsSave, "admin-web"),
	postCase("admin actors admin permissions save missing", actorsAdminPermsSave, "admin-web", `{"actor_id":1}`),
	postCase("admin actors admin permissions save unknown actor", actorsAdminPermsSave, "admin-web", `{"actor_id":999999,"permissions":[]}`),
	withIDs("admin actors admin permissions save filters and takes effect",
		limitedIDs(),
		adminPost(adminConfigListPath, "admin-limited-web", `{}`),
		adminPost(actorsAdminPermsSave, "admin-web",
			`{"actor_id":{{limited_id}},"permissions":[{"resource":"config","action":"read"},{"resource":"config","action":"read"},{"resource":" agents ","action":" write "},{"resource":"bogus","action":"read"},{"resource":"agents","action":"administer"},{"resource":5,"action":"x"},{"resource":"","action":"read"},"text",{"resource":"plugins","action":"administer"}]}`),
		adminPost(actorsAdminPermsRead, "admin-web", `{"actor_id":{{limited_id}}}`),
		adminPost(adminConfigListPath, "admin-limited-web", `{}`),
		adminPost(authVerifyPath, "", `{"token":"{{admin-limited-web}}"}`)),
	withIDs("admin actors admin permissions save own",
		verifyID("me", "admin-web"),
		adminPost(actorsAdminPermsSave, "admin-web", `{"actor_id":{{me}},"permissions":[{"resource":"*","action":"*"},{"resource":"logs","action":"read"}]}`)),
	withIDs("admin actors admin permissions save inherited name",
		limitedIDs(),
		adminPost(actorsAdminPermsSave, "admin-web", `{"actor_id":{{limited_id}},"permissions":[{"resource":"constructor","action":"read"}]}`),
		adminPost(actorsAdminPermsRead, "admin-web", `{"actor_id":{{limited_id}}}`)),
	withIDs("admin actors admin permissions save null entry",
		limitedIDs(),
		adminPost(actorsAdminPermsSave, "admin-web", `{"actor_id":{{limited_id}},"permissions":[null]}`)),
}

var adminActorDeleteCases = []Scenario{
	// helper-va (5th in the superadmin's list, by name) has mail, chat
	// and discussion participation, but created no discussion.
	withIDs("admin actors delete an agent with mail and discussions",
		Step{Method: "POST", Path: actorsListPath, Auth: "admin-web", Header: jsonType, Body: `{}`,
			Capture: map[string]string{"helper": "actors.4.id"}},
		adminPost(actorsDeletePath, "admin-web", `{"actor_id":{{helper}}}`),
		adminPost(actorsListPath, "admin-web", `{}`),
		Step{Settle: true}),
	// agent-b created discussions that other actors joined: Node deletes
	// the discussions before their other participants, so the foreign key
	// refuses it and the transaction rolls back (in Go too).
	withIDs("admin actors delete an agent with history",
		verifyID("b", "agent-b-session"),
		adminPost(actorsDeletePath, "admin-web", `{"actor_id":{{b}}}`),
		adminPost(actorsListPath, "admin-web", `{}`),
		Step{Settle: true}),
	withIDs("admin actors delete cascades to created virtual agents",
		limitedIDs(),
		adminPost(actorsDeletePath, "admin-web", `{"actor_id":"{{limited_id}}"}`),
		adminPost(actorsListPath, "admin-web", `{}`)),
	{
		// Node kept answering a deleted actor's cached session (its
		// permission check then failed); Go drops the cached token.
		Name: "admin actors delete ends the cached session",
		Steps: []Step{
			limitedIDs(),
			adminPost(actorsDeletePath, "admin-web", `{"actor_id":{{limited_id}}}`),
			adminPost(actorsListPath, "admin-limited-web", `{}`),
		},
		Known: []KnownDifference{
			{Where: "step 3 POST " + actorsListPath + ": body.error.message", Reason: "a deleted actor's cached sessions are dropped at once in Go (go-server note, Known differences)"},
			{Where: "table request_log*", Reason: "Go refuses the token in the auth middleware, so its log row has no actor (go-server note, Known differences)"},
		},
	},
	withIDs("admin actors delete refusals",
		verifyID("me", "admin-web"),
		verifyID("a", "agent-a-session"),
		adminPost(actorsDeletePath, "admin-web", `{"actor_id":{{me}}}`),
		adminPost(actorsDeletePath, "admin-web", `{"actor_id":999999}`),
		adminPost(actorsDeletePath, "admin-web", `{"actor_id":"x"}`),
		adminPost(actorsDeletePath, "admin-limited-web", `{"actor_id":{{a}}}`)),
	noBodyPost("admin actors delete no body", actorsDeletePath, "admin-web"),
}

var adminActorSearchCases = []Scenario{
	postCase("admin actors searchable", actorsSearchablePath, "admin-web", `{"query":"LIM"}`),
	postCase("admin actors searchable excludes oneself", actorsSearchablePath, "admin-limited-web", `{}`),
	postCase("admin actors searchable wildcard characters", actorsSearchablePath, "admin-web", `{"query":"%_"}`),
	noBodyPost("admin actors searchable no body", actorsSearchablePath, "admin-web"),
	postCase("admin actors searchable number query", actorsSearchablePath, "admin-web", `{"query":5}`,
		notAStringInAdminSession("POST "+actorsSearchablePath)...),
}

var adminVAAccessCases = []Scenario{
	postCase("admin va access list", vaAccessListPath, "admin-limited-web", `{}`),
	withIDs("admin va access grant and revoke",
		limitedIDs(),
		adminPost(vaAccessGrantPath, "admin-limited-web", `{"virtual_agent_id":{{limited_va_id}},"grantee_actor_id":{{limited_agent_id}}}`),
		adminPost(vaAccessGrantPath, "admin-limited-web", `{"virtual_agent_id":{{limited_va_id}},"grantee_actor_id":{{limited_agent_id}}}`),
		adminPost(vaAccessGrantPath, "admin-limited-web", `{"virtual_agent_id":"{{limited_va_id}}"}`),
		Step{Method: "POST", Path: vaAccessListPath, Auth: "admin-web", Header: jsonType, Body: `{}`,
			Capture: map[string]string{"access": "access.0.id", "other": "access.2.id"}},
		adminPost(vaAccessRevokePath, "admin-limited-web", `{"id":{{access}}}`),
		adminPost(vaAccessRevokePath, "admin-limited-web", `{"id":{{other}}}`),
		adminPost(vaAccessRevokePath, "admin-limited-web", `{"id":"{{access}}"}`),
		adminPost(vaAccessListPath, "admin-web", `{}`)),
	withIDs("admin va access grant refusals",
		verifyID("a", "agent-a-session"),
		Step{Method: "POST", Path: vaAccessListPath, Auth: "admin-web", Header: jsonType, Body: `{}`,
			Capture: map[string]string{"va": "access.0.virtual_agent_id"}},
		adminPost(vaAccessGrantPath, "admin-limited-web", `{}`),
		adminPost(vaAccessGrantPath, "admin-limited-web", `{"virtual_agent_id":{{a}}}`),
		adminPost(vaAccessGrantPath, "admin-limited-web", `{"virtual_agent_id":{{va}}}`),
		adminPost(vaAccessGrantPath, "admin-web", `{"virtual_agent_id":"abc"}`)),
	postCase("admin va access revoke missing id", vaAccessRevokePath, "admin-web", `{}`),
	postCase("admin va access revoke unknown", vaAccessRevokePath, "admin-web", `{"id":999999}`),
	noBodyPost("admin va access grant no body", vaAccessGrantPath, "admin-web"),
	noBodyPost("admin va access revoke no body", vaAccessRevokePath, "admin-web"),
}
