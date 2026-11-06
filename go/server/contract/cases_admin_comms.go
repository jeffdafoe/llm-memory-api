package contract

// adminCommsCases cover the admin comms views (discussions, mail, chat),
// the dashboard, the two logs and the signup funnel. contract-admin is a
// superadmin and sees every actor; the limited admin (dashboard:read, no
// realm) sees only itself, which exercises the visibility filters where it
// may call the route.
var adminCommsCases = concat(adminDiscussionCases, adminMailChatCases, adminLogCases)

const (
	adminDiscussions        = "/v1/admin/discussions"
	adminDiscussionDetail   = "/v1/admin/discussions/detail"
	adminDiscussionCreate   = "/v1/admin/discussions/create"
	adminDiscussionSend     = "/v1/admin/discussions/send"
	adminDiscussionConclude = "/v1/admin/discussions/conclude"
	adminChat               = "/v1/admin/chat"
	adminChatDelete         = "/v1/admin/chat/delete"
	adminMail               = "/v1/admin/mail"
	adminMailDelete         = "/v1/admin/mail/delete"
	adminMailSend           = "/v1/admin/mail/send"
	adminDashboard          = "/v1/admin/dashboard"
	adminAPILog             = "/v1/admin/api-log"
	adminErrorLog           = "/v1/admin/error-log"
	adminFunnel             = "/v1/admin/funnel"
)

// limitedPost is a JSON POST as the limited admin.
func limitedPost(path, body string) Step {
	return Step{Method: "POST", Path: path, Auth: "admin-limited-web", Header: jsonType, Body: body}
}

// grantLimited gives the limited admin one more admin grant. Like
// grantAdmin it must come before the limited admin's first request.
func grantLimited(resource, action string) Step {
	return Step{SQL: `INSERT INTO admin_permissions (actor_id, resource, action)
         SELECT id, '` + resource + `', '` + action + `' FROM actors WHERE name = 'contract-admin-limited'`}
}

var adminDiscussionCases = []Scenario{
	{Name: "admin discussions list", Steps: steps(routeShape(adminDiscussions), []Step{
		adminPost(adminDiscussions, `{}`),
		adminPost(adminDiscussions, `{"status":"active"}`),
		adminPost(adminDiscussions, `{"status":"no-such"}`),
		adminPost(adminDiscussions, `{"status":""}`),
	})},
	{Name: "admin discussions list filtered", Steps: []Step{
		grantLimited("comms", "read"),
		limitedPost(adminDiscussions, `{}`),
		limitedPost(adminDiscussionDetail, `{"discussion_id":101}`),
	}},
	{Name: "admin discussions detail", Steps: steps(routeShape(adminDiscussionDetail), []Step{
		adminPost(adminDiscussionDetail, `{}`),
		adminPost(adminDiscussionDetail, `{"discussion_id":105}`),
		adminPost(adminDiscussionDetail, `{"discussion_id":"101"}`),
		adminPost(adminDiscussionDetail, `{"discussion_id":104}`),
		adminPost(adminDiscussionDetail, `{"discussion_id":999}`),
		adminPost(adminDiscussionDetail, `{"discussion_id":"abc"}`),
	})},
	{Name: "admin discussions create", Steps: steps(routeShape(adminDiscussionCreate), []Step{
		adminPost(adminDiscussionCreate, `{}`),
		adminPost(adminDiscussionCreate, `{"topic":"  ","participants":["agent-a"]}`),
		adminPost(adminDiscussionCreate, `{"topic":"T","participants":[]}`),
		adminPost(adminDiscussionCreate, `{"topic":"T","participants":["",null,0]}`),
		adminPost(adminDiscussionCreate, `{"topic":"  Admin topic  ","participants":["agent-a","agent-a","",null],"mode":"async","context":"Some context"}`),
		{Settle: true},
		adminPost(adminDiscussionCreate, `{"topic":"T","participants":["nobody-at-all"]}`),
		{Settle: true},
	})},
	{Name: "admin discussions create naming the admin", Steps: []Step{
		adminPost(adminDiscussionCreate, `{"topic":"With me","participants":["agent-b","contract-admin"],"mode":"realtime"}`),
		{Settle: true},
	}},
	{Name: "admin discussions send", Steps: steps(routeShape(adminDiscussionSend), []Step{
		adminPost(adminDiscussionSend, `{"discussion_id":101}`),
		adminPost(adminDiscussionSend, `{"message":"hi"}`),
		adminPost(adminDiscussionSend, `{"discussion_id":101,"message":"   "}`),
		// contract-admin is not a participant of 101: Node's chatSend sends
		// to the joined participants anyway.
		adminPost(adminDiscussionSend, `{"discussion_id":101,"message":"  From the dashboard  "}`),
		{Settle: true},
		adminPost(adminDiscussionSend, `{"discussion_id":"abc","message":"x"}`),
		adminPost(adminDiscussionSend, `{"discussion_id":99999999999,"message":"x"}`),
	})},
	{Name: "admin discussions conclude", Steps: steps(routeShape(adminDiscussionConclude), []Step{
		adminPost(adminDiscussionConclude, `{}`),
		// The admin has not joined fixture discussions.
		adminPost(adminDiscussionConclude, `{"discussion_id":105}`),
		{Method: "POST", Path: adminDiscussionCreate, Auth: "admin-web", Header: jsonType,
			Body:    `{"topic":"To end","participants":["agent-a"]}`,
			Capture: map[string]string{"did": "discussion.id"}},
		{Settle: true},
		adminPost(adminDiscussionConclude, `{"discussion_id":{{did}}}`),
		{Settle: true},
	}), Known: deliveryRace},
}

var adminMailChatCases = []Scenario{
	{Name: "admin mail list", Steps: steps(routeShape(adminMail), []Step{
		adminPost(adminMail, `{}`),
		adminPost(adminMail, `{"limit":2}`),
		adminPost(adminMail, `{"limit":null}`),
		adminPost(adminMail, `{"limit":"x"}`),
	})},
	{Name: "admin mail list filtered", Steps: []Step{
		grantLimited("comms", "read"),
		limitedPost(adminMail, `{}`),
		limitedPost(adminChat, `{}`),
	}},
	{Name: "admin mail delete", Steps: steps(routeShape(adminMailDelete), []Step{
		adminPost(adminMailDelete, `{}`),
		adminPost(adminMailDelete, `{"id":"6d2a0c1e-0000-4000-8000-0000000000a1"}`),
		adminPost(adminMailDelete, `{"id":"6d2a0c1e-0000-4000-8000-0000000000a1"}`),
		adminPost(adminMailDelete, `{"id":"not-a-uuid"}`),
	})},
	{Name: "admin mail delete filtered", Steps: []Step{
		grantLimited("comms", "delete"),
		limitedPost(adminMailDelete, `{"id":"6d2a0c1e-0000-4000-8000-0000000000a1"}`),
	}},
	{Name: "admin mail send", Steps: steps(routeShape(adminMailSend), []Step{
		adminPost(adminMailSend, `{"to":"agent-a"}`),
		adminPost(adminMailSend, `{"to":"  Agent-A! ","subject":"Hello`+bs+`u200b","body":"From the dashboard."}`),
		adminPost(adminMailSend, `{"to":"nobody-at-all","subject":"s","body":"b"}`),
		adminPost(adminMailSend, `{"to":"helper-va","subject":"s","body":"b"}`),
		{Settle: true},
	})},
	{Name: "admin mail send filtered", Steps: []Step{
		grantLimited("comms", "write"),
		limitedPost(adminMailSend, `{"to":"agent-a","subject":"s","body":"b"}`),
	}},
	{Name: "admin chat list", Steps: steps(routeShape(adminChat), []Step{
		adminPost(adminChat, `{}`),
		adminPost(adminChat, `{"limit":1}`),
		adminPost(adminChat, `{"discussion_id":101}`),
		adminPost(adminChat, `{"discussion_id":"101","limit":2}`),
		adminPost(adminChat, `{"discussion_id":0}`),
		adminPost(adminChat, `{"discussion_id":"abc"}`),
	})},
	{Name: "admin chat delete", Steps: steps(routeShape(adminChatDelete), []Step{
		adminPost(adminChatDelete, `{}`),
		adminPost(adminChatDelete, `{"id":101}`),
		adminPost(adminChatDelete, `{"id":101}`),
		adminPost(adminChatDelete, `{"id":"x"}`),
	})},
}

// apiLogDuration is the request duration in each api-log entry, which
// differs between two correct runs.
func apiLogDuration(step int) KnownDifference {
	return KnownDifference{
		Where:    "step " + itoa(step) + " POST /v1/admin/api-log: body.entries.*.duration",
		Reason:   "the request's own duration in milliseconds",
		Volatile: true,
	}
}

var adminLogCases = []Scenario{
	{Name: "admin dashboard", Steps: steps(routeShape(adminDashboard), []Step{
		adminPost(adminDashboard, `{}`),
		// The limited admin sees itself only.
		limitedPost(adminDashboard, `{}`),
	})},
	{
		Name: "admin api log",
		Steps: steps(routeShape(adminAPILog), []Step{
			{Method: "POST", Path: noRoutePath, Auth: "agent-a-session", Header: jsonType, Body: `{}`},
			{Method: "GET", Path: "/health"},
			{Settle: true},
			adminPost(adminAPILog, `{}`),
			adminPost(adminAPILog, `{"limit":1}`),
			adminPost(adminAPILog, `{"since_id":1}`),
			adminPost(adminAPILog, `{"since_id":"x"}`),
		}),
		Known: []KnownDifference{apiLogDuration(9), apiLogDuration(10), apiLogDuration(11)},
	},
	{Name: "admin api log filtered", Steps: []Step{
		grantLimited("logs", "read"),
		{Method: "POST", Path: noRoutePath, Auth: "agent-a-session", Header: jsonType, Body: `{}`},
		{Settle: true},
		limitedPost(adminAPILog, `{}`),
		limitedPost(adminErrorLog, `{}`),
	}},
	{
		Name: "admin error log",
		Steps: steps(routeShape(adminErrorLog), []Step{
			// A logged 404 (attributed to agent-a) and a logged 400.
			{Method: "POST", Path: readPath, Auth: "agent-a-session", Header: jsonType, Body: `{"namespace":"agent-a","slug":"no/such"}`},
			adminPost(adminNotesSave, `{"namespace":"contract-admin","slug":"x","title":"T","content":""}`),
			{Settle: true},
			adminPost(adminErrorLog, `{}`),
			adminPost(adminErrorLog, `{"limit":1}`),
			adminPost(adminErrorLog, `{"since_id":1}`),
		}),
		Known: []KnownDifference{{
			Where:  "step * POST /v1/admin/error-log: body.entries.*.error_detail",
			Reason: "error_detail is Node's stack trace and Go's %+v of the error (go-server note, Known differences)",
		}},
	},
	{Name: "admin funnel", Steps: steps(routeShape(adminFunnel), []Step{
		adminPost(adminFunnel, `{}`),
		{SQL: `INSERT INTO signup_events (actor_id, event, value)
               SELECT id, 'guide_shown', 'claude-code' FROM actors WHERE name = 'agent-a'`},
		adminPost(adminFunnel, `{}`),
	})},
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
