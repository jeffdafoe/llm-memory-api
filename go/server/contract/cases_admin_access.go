package contract

// adminAccessCases cover /admin/templates/*, /admin/access-requests* and
// /admin/invite-codes*. Invite codes are 32 random hex characters, which
// the normalizer names like any random value.
var adminAccessCases = concat(adminTemplateCases, adminAccessRequestCases, adminInviteCodeCases)

const (
	templatesListPath     = "/v1/admin/templates/list"
	templatesReadPath     = "/v1/admin/templates/read"
	templatesSavePath     = "/v1/admin/templates/save"
	templatesDeletePath   = "/v1/admin/templates/delete"
	accessRequestsPath    = "/v1/admin/access-requests"
	accessApprovePath     = "/v1/admin/access-requests/approve"
	accessRejectPath      = "/v1/admin/access-requests/reject"
	inviteCodesPath       = "/v1/admin/invite-codes"
	inviteCodesGenerate   = "/v1/admin/invite-codes/generate"
	inviteCodesDeletePath = "/v1/admin/invite-codes/delete"
)

// pendingRequests adds two pending access requests (9001, 9002).
var pendingRequests = Step{SQL: `
    INSERT INTO access_requests (id, email, usage_description, status, created_at) VALUES
      (9001, 'first@example.com', 'Notes for a project.', 'pending', NOW() - INTERVAL '2 days'),
      (9002, 'second@example.com', 'Trying it out.', 'pending', NOW() - INTERVAL '1 day');`}

var adminTemplateCases = []Scenario{
	postCase("admin templates list", templatesListPath, "admin-web", `{}`),
	postCase("admin templates list one kind", templatesListPath, "admin-web", `{"kind":"welcome-note"}`),
	postCase("admin templates list invalid kind", templatesListPath, "admin-web", `{"kind":"note"}`),
	postCase("admin templates list number kind", templatesListPath, "admin-web", `{"kind":5}`),
	postCase("admin templates list needs templates read", templatesListPath, "admin-limited-web", `{}`),
	noBodyPost("admin templates list no body", templatesListPath, "admin-web"),
	withIDs("admin templates read",
		Step{Method: "POST", Path: templatesListPath, Auth: "admin-web", Header: jsonType, Body: `{}`,
			Capture: map[string]string{"t": "templates.0.id"}},
		adminPost(templatesReadPath, "admin-web", `{"id":{{t}}}`),
		adminPost(templatesReadPath, "admin-web", `{"id":"{{t}}"}`),
		adminPost(templatesReadPath, "admin-web", `{"id":999999}`),
		adminPost(templatesReadPath, "admin-web", `{"id":"abc"}`),
		adminPost(templatesReadPath, "admin-web", `{}`)),
	noBodyPost("admin templates read no body", templatesReadPath, "admin-web"),
	withIDs("admin templates save create and update",
		Step{Method: "POST", Path: templatesSavePath, Auth: "admin-web", Header: jsonType,
			Body:    `{"name":"Contract welcome​","content":"---\nsubject: Hi {agent}\n---\n\nHello.","description":"First."}`,
			Capture: map[string]string{"t": "template.id"}},
		adminPost(templatesSavePath, "admin-web", `{"name":"Contract note","kind":"welcome-note","content":"No header here.","description":""}`),
		adminPost(templatesSavePath, "admin-web", `{"id":{{t}},"name":"Contract welcome 2","kind":"welcome","content":"---\nsubject: Bye\n---\nBody","description":0}`),
		adminPost(templatesReadPath, "admin-web", `{"id":{{t}}}`),
		adminPost(templatesListPath, "admin-web", `{"kind":"welcome"}`)),
	withIDs("admin templates save refusals",
		adminPost(templatesSavePath, "admin-web", `{"name":"x"}`),
		adminPost(templatesSavePath, "admin-web", `{"name":"​","content":"c"}`),
		adminPost(templatesSavePath, "admin-web", `{"name":"x","content":"c","kind":"note"}`),
		adminPost(templatesSavePath, "admin-web", `{"name":"x","content":"no header"}`),
		adminPost(templatesSavePath, "admin-web", `{"name":"x","content":"---\nsubject:\n---\nB"}`),
		adminPost(templatesSavePath, "admin-web", `{"id":999999,"name":"x","kind":"welcome-note","content":"c"}`),
		Step{Method: "POST", Path: templatesListPath, Auth: "admin-web", Header: jsonType, Body: `{}`,
			Capture: map[string]string{"n": "templates.0.name"}},
		adminPost(templatesSavePath, "admin-web", `{"name":"{{n}}","kind":"welcome-note","content":"c"}`),
		adminPost(templatesSavePath, "admin-limited-web", `{"name":"x","kind":"welcome-note","content":"c"}`)),
	noBodyPost("admin templates save no body", templatesSavePath, "admin-web"),
	postCase("admin templates save number name", templatesSavePath, "admin-web", `{"name":5,"kind":"welcome-note","content":"c"}`,
		append(notAStringInAdminSession("POST "+templatesSavePath),
			KnownDifference{Where: "table templates*", Reason: "Node stored the number as the name; Go refuses it (go-server note, Known differences)"})...),
	withIDs("admin templates delete",
		Step{Method: "POST", Path: templatesSavePath, Auth: "admin-web", Header: jsonType,
			Body:    `{"name":"Doomed","kind":"welcome-note","content":"c"}`,
			Capture: map[string]string{"t": "template.id"}},
		adminPost(templatesDeletePath, "admin-limited-web", `{"id":{{t}}}`),
		adminPost(templatesDeletePath, "admin-web", `{"id":"{{t}}"}`),
		adminPost(templatesDeletePath, "admin-web", `{"id":{{t}}}`),
		adminPost(templatesDeletePath, "admin-web", `{"id":0}`)),
	noBodyPost("admin templates delete no body", templatesDeletePath, "admin-web"),
}

var adminAccessRequestCases = []Scenario{
	withIDs("admin access requests list",
		pendingRequests,
		adminPost(accessRequestsPath, "admin-web", `{}`),
		adminPost(accessRequestsPath, "admin-web", `{"status":"pending"}`),
		adminPost(accessRequestsPath, "admin-web", `{"status":5}`)),
	postCase("admin access requests needs access read", accessRequestsPath, "admin-limited-web", `{}`),
	postCase("admin access requests as an agent", accessRequestsPath, "agent-a-session", `{}`),
	noBodyPost("admin access requests no body", accessRequestsPath, "admin-web"),
	withIDs("admin access requests approve and reject",
		pendingRequests,
		adminPost(accessApprovePath, "admin-web", `{"id":9001,"notes":"Welcome aboard."}`),
		adminPost(accessApprovePath, "admin-web", `{"id":"9001"}`),
		adminPost(accessRejectPath, "admin-web", `{"id":9002,"notes":""}`),
		adminPost(accessRejectPath, "admin-web", `{"id":9002}`),
		adminPost(accessRequestsPath, "admin-web", `{}`),
		adminPost(inviteCodesPath, "admin-web", `{}`)),
	withIDs("admin access requests approve in the approver's realm",
		pendingRequests,
		Step{SQL: `UPDATE actors SET realms = '{}' WHERE name = 'contract-admin'`},
		adminPost(accessApprovePath, "admin-web", `{"id":9002}`),
		adminPost(inviteCodesPath, "admin-web", `{}`)),
	withIDs("admin access requests refusals",
		pendingRequests,
		adminPost(accessApprovePath, "admin-web", `{}`),
		adminPost(accessApprovePath, "admin-web", `{"id":999999}`),
		adminPost(accessApprovePath, "admin-web", `{"id":"abc"}`),
		adminPost(accessRejectPath, "admin-web", `{"id":999999}`),
		adminPost(accessApprovePath, "admin-limited-web", `{"id":9001}`),
		adminPost(accessRejectPath, "admin-limited-web", `{"id":9001}`)),
	noBodyPost("admin access requests approve no body", accessApprovePath, "admin-web"),
	noBodyPost("admin access requests reject no body", accessRejectPath, "admin-web"),
}

var adminInviteCodeCases = []Scenario{
	postCase("admin invite codes list", inviteCodesPath, "admin-web", `{}`),
	noBodyPost("admin invite codes list no body", inviteCodesPath, "admin-web"),
	postCase("admin invite codes needs access read", inviteCodesPath, "admin-limited-web", `{}`),
	withIDs("admin invite codes generate",
		adminPost(inviteCodesGenerate, "admin-web", `{"count":3,"expires_days":"7","realm":"other"}`),
		adminPost(inviteCodesGenerate, "admin-web", `{}`),
		adminPost(inviteCodesGenerate, "admin-web", `{"count":"many","expires_days":null,"realm":""}`),
		adminPost(inviteCodesGenerate, "admin-web", `{"count":-5,"expires_days":""}`),
		adminPost(inviteCodesPath, "admin-web", `{}`)),
	withIDs("admin invite codes generate the most",
		adminPost(inviteCodesGenerate, "admin-web", `{"count":500}`)),
	withIDs("admin invite codes generate refusals",
		adminPost(inviteCodesGenerate, "admin-web", `{"expires_days":0}`),
		adminPost(inviteCodesGenerate, "admin-web", `{"expires_days":3651}`),
		adminPost(inviteCodesGenerate, "admin-web", `{"expires_days":"2.5"}`),
		adminPost(inviteCodesGenerate, "admin-web", `{"expires_days":true}`),
		adminPost(inviteCodesGenerate, "admin-limited-web", `{}`)),
	noBodyPost("admin invite codes generate no body", inviteCodesGenerate, "admin-web"),
	withIDs("admin invite codes delete",
		adminPost(inviteCodesGenerate, "admin-web", `{}`),
		Step{Method: "POST", Path: inviteCodesPath, Auth: "admin-web", Header: jsonType, Body: `{}`,
			Capture: map[string]string{"c": "codes.0.id"}},
		adminPost(inviteCodesDeletePath, "admin-limited-web", `{"id":{{c}}}`),
		adminPost(inviteCodesDeletePath, "admin-web", `{"id":"{{c}}"}`),
		adminPost(inviteCodesDeletePath, "admin-web", `{"id":{{c}}}`),
		adminPost(inviteCodesDeletePath, "admin-web", `{}`),
		adminPost(inviteCodesPath, "admin-web", `{}`)),
	withIDs("admin invite codes delete a used code",
		Step{SQL: `UPDATE invite_codes SET id = 9001 WHERE code = 'contract-used-invite'`},
		adminPost(inviteCodesDeletePath, "admin-web", `{"id":9001}`)),
	noBodyPost("admin invite codes delete no body", inviteCodesDeletePath, "admin-web"),
}
