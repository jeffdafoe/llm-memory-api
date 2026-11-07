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
		contentPost(adminDiscussions, `{}`),
		contentPost(adminDiscussions, `{"status":"active"}`),
		contentPost(adminDiscussions, `{"status":"no-such"}`),
		contentPost(adminDiscussions, `{"status":""}`),
	})},
	{Name: "admin discussions list filtered", Steps: []Step{
		grantLimited("comms", "read"),
		limitedPost(adminDiscussions, `{}`),
		limitedPost(adminDiscussionDetail, `{"discussion_id":101}`),
	}},
	{Name: "admin discussions detail", Steps: steps(routeShape(adminDiscussionDetail), []Step{
		contentPost(adminDiscussionDetail, `{}`),
		contentPost(adminDiscussionDetail, `{"discussion_id":105}`),
		contentPost(adminDiscussionDetail, `{"discussion_id":"101"}`),
		contentPost(adminDiscussionDetail, `{"discussion_id":104}`),
		contentPost(adminDiscussionDetail, `{"discussion_id":999}`),
		contentPost(adminDiscussionDetail, `{"discussion_id":"abc"}`),
	})},
	{Name: "admin discussions create", Steps: steps(routeShape(adminDiscussionCreate), []Step{
		contentPost(adminDiscussionCreate, `{}`),
		contentPost(adminDiscussionCreate, `{"topic":"  ","participants":["agent-a"]}`),
		contentPost(adminDiscussionCreate, `{"topic":"T","participants":[]}`),
		contentPost(adminDiscussionCreate, `{"topic":"T","participants":["",null,0]}`),
		contentPost(adminDiscussionCreate, `{"topic":"  Admin topic  ","participants":["agent-a","agent-a","",null],"mode":"async","context":"Some context"}`),
		{Settle: true},
		contentPost(adminDiscussionCreate, `{"topic":"T","participants":["nobody-at-all"]}`),
		{Settle: true},
	})},
	{Name: "admin discussions create naming the admin", Steps: []Step{
		contentPost(adminDiscussionCreate, `{"topic":"With me","participants":["agent-b","contract-admin"],"mode":"realtime"}`),
		{Settle: true},
	}},
	{Name: "admin discussions send", Steps: steps(routeShape(adminDiscussionSend), []Step{
		contentPost(adminDiscussionSend, `{"discussion_id":101}`),
		contentPost(adminDiscussionSend, `{"message":"hi"}`),
		contentPost(adminDiscussionSend, `{"discussion_id":101,"message":"   "}`),
		// contract-admin is not a participant of 101: Node's chatSend sends
		// to the joined participants anyway.
		contentPost(adminDiscussionSend, `{"discussion_id":101,"message":"  From the dashboard  "}`),
		{Settle: true},
		contentPost(adminDiscussionSend, `{"discussion_id":"abc","message":"x"}`),
		contentPost(adminDiscussionSend, `{"discussion_id":99999999999,"message":"x"}`),
	})},
	{Name: "admin discussions conclude", Steps: steps(routeShape(adminDiscussionConclude), []Step{
		contentPost(adminDiscussionConclude, `{}`),
		// The admin has not joined fixture discussions.
		contentPost(adminDiscussionConclude, `{"discussion_id":105}`),
		{Method: "POST", Path: adminDiscussionCreate, Auth: "admin-web", Header: jsonType,
			Body:    `{"topic":"To end","participants":["agent-a"]}`,
			Capture: map[string]string{"did": "discussion.id"}},
		{Settle: true},
		contentPost(adminDiscussionConclude, `{"discussion_id":{{did}}}`),
		{Settle: true},
	}), Known: deliveryRace},
}

var adminMailChatCases = []Scenario{
	{Name: "admin mail list", Steps: steps(routeShape(adminMail), []Step{
		contentPost(adminMail, `{}`),
		contentPost(adminMail, `{"limit":2}`),
		contentPost(adminMail, `{"limit":null}`),
		contentPost(adminMail, `{"limit":"x"}`),
	})},
	{Name: "admin mail list filtered", Steps: []Step{
		grantLimited("comms", "read"),
		limitedPost(adminMail, `{}`),
		limitedPost(adminChat, `{}`),
	}},
	{Name: "admin mail delete", Steps: steps(routeShape(adminMailDelete), []Step{
		contentPost(adminMailDelete, `{}`),
		contentPost(adminMailDelete, `{"id":"6d2a0c1e-0000-4000-8000-0000000000a1"}`),
		contentPost(adminMailDelete, `{"id":"6d2a0c1e-0000-4000-8000-0000000000a1"}`),
		contentPost(adminMailDelete, `{"id":"not-a-uuid"}`),
	})},
	{Name: "admin mail delete filtered", Steps: []Step{
		grantLimited("comms", "delete"),
		limitedPost(adminMailDelete, `{"id":"6d2a0c1e-0000-4000-8000-0000000000a1"}`),
	}},
	{Name: "admin mail send", Steps: steps(routeShape(adminMailSend), []Step{
		contentPost(adminMailSend, `{"to":"agent-a"}`),
		contentPost(adminMailSend, `{"to":"  Agent-A! ","subject":"Hello`+bs+`u200b","body":"From the dashboard."}`),
		contentPost(adminMailSend, `{"to":"nobody-at-all","subject":"s","body":"b"}`),
		contentPost(adminMailSend, `{"to":"helper-va","subject":"s","body":"b"}`),
		{Settle: true},
	})},
	{Name: "admin mail send filtered", Steps: []Step{
		grantLimited("comms", "write"),
		limitedPost(adminMailSend, `{"to":"agent-a","subject":"s","body":"b"}`),
	}},
	{Name: "admin chat list", Steps: steps(routeShape(adminChat), []Step{
		contentPost(adminChat, `{}`),
		contentPost(adminChat, `{"limit":1}`),
		contentPost(adminChat, `{"discussion_id":101}`),
		contentPost(adminChat, `{"discussion_id":"101","limit":2}`),
		contentPost(adminChat, `{"discussion_id":0}`),
		contentPost(adminChat, `{"discussion_id":"abc"}`),
	})},
	{Name: "admin chat delete", Steps: steps(routeShape(adminChatDelete), []Step{
		contentPost(adminChatDelete, `{}`),
		contentPost(adminChatDelete, `{"id":101}`),
		contentPost(adminChatDelete, `{"id":101}`),
		contentPost(adminChatDelete, `{"id":"x"}`),
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
		// Wait for routeShape's background heartbeats before reading the
		// dashboard, whose agent status and ordering depend on last_seen.
		{Settle: true},
		contentPost(adminDashboard, `{}`),
		// The limited admin sees itself only.
		limitedPost(adminDashboard, `{}`),
	})},
	{
		Name: "admin api log",
		// The listing is in request_log id order, and each row is written in
		// the background, so every request settles before the next.
		Steps: settleEach(steps(routeShape(adminAPILog), []Step{
			{Method: "POST", Path: noRoutePath, Auth: "agent-a-session", Header: jsonType, Body: `{}`},
			{Method: "GET", Path: "/health"},
			contentPost(adminAPILog, `{}`),
			contentPost(adminAPILog, `{"limit":1}`),
			contentPost(adminAPILog, `{"since_id":1}`),
			contentPost(adminAPILog, `{"since_id":"x"}`),
		})),
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
		// error_log rows are written in the background too.
		Steps: settleEach(steps(routeShape(adminErrorLog), []Step{
			// A logged 404 (attributed to agent-a) and a logged 400.
			{Method: "POST", Path: readPath, Auth: "agent-a-session", Header: jsonType, Body: `{"namespace":"agent-a","slug":"no/such"}`},
			contentPost(adminNotesSave, `{"namespace":"contract-admin","slug":"x","title":"T","content":""}`),
			{Settle: true},
			contentPost(adminErrorLog, `{}`),
			contentPost(adminErrorLog, `{"limit":1}`),
			contentPost(adminErrorLog, `{"since_id":1}`),
		})),
		Known: []KnownDifference{{
			Where:  "step * POST /v1/admin/error-log: body.entries.*.error_detail",
			Reason: "error_detail is Node's stack trace and Go's %+v of the error (go-server note, Known differences)",
		}},
	},
	{Name: "admin funnel", Steps: steps(routeShape(adminFunnel), []Step{
		contentPost(adminFunnel, `{}`),
		{SQL: `INSERT INTO signup_events (actor_id, event, value)
               SELECT id, 'guide_shown', 'claude-code' FROM actors WHERE name = 'agent-a'`},
		contentPost(adminFunnel, `{}`),
	})},
}

// settleEach puts a settle after every step.
func settleEach(in []Step) []Step {
	var out []Step
	for _, st := range in {
		out = append(out, st, Step{Settle: true})
	}
	return out
}

func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
