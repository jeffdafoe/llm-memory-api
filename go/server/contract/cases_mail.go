package contract

import "strings"

// mailCases cover the /v1/mail routes against the fixture mail
// (fixture_mail.go). The fixture ids are written out here because this file
// has no build tag.
var mailCases = concat(mailSendCases, mailReadCases, mailAckCases, mailEditUnsendCases, mailListCases, mailRoutingCases)

const (
	mailSendPath    = "/v1/mail/send"
	mailCheckPath   = "/v1/mail/check"
	mailReceivePath = "/v1/mail/receive"
	mailAckPath     = "/v1/mail/ack"
	mailEditPath    = "/v1/mail/edit"
	mailUnsendPath  = "/v1/mail/unsend"
	mailSentPath    = "/v1/mail/sent"
	mailHistoryPath = "/v1/mail/history"

	fixtureUnreadToA = "6d2a0c1e-0000-4000-8000-0000000000a1"
	fixtureReadByA   = "6d2a0c1e-0000-4000-8000-0000000000a2"
	fixtureUnreadToB = "6d2a0c1e-0000-4000-8000-0000000000b1"
	fixtureUnsentToB = "6d2a0c1e-0000-4000-8000-0000000000b2"
	fixtureReadByB   = "6d2a0c1e-0000-4000-8000-0000000000b3"
)

// notAStringInAdminSession is the known difference for a truthy non-string
// field: Go refuses it with 400; Node used it as text (here an actor name
// no actor has, so a 404).
func notAStringInAdminSession(label string) []KnownDifference {
	return []KnownDifference{
		{Where: "step 1 " + label + ": *", Reason: "a request-body field of a JSON type the route cannot use is a 400 in Go (go-server note, Known differences)"},
		{Where: "table error_log*", Reason: "the same refusal is logged with Go's status and message (go-server note, Known differences)"},
		{Where: "table request_log*", Reason: "the request log records the status and length the client got (go-server note, Known differences)"},
	}
}

var mailSendCases = []Scenario{
	postCase("mail send", mailSendPath, "agent-a-session",
		`{"to_agent":"agent-b","subject":"Hello","body":"Body text."}`),
	postCase("mail send as a reply", mailSendPath, "agent-a-session",
		`{"to_agent":"agent-b","subject":"Re: Fixture unread","body":"Answer.","in_reply_to":"`+fixtureUnreadToA+`"}`),
	postCase("mail send in_reply_to not a uuid", mailSendPath, "agent-a-session",
		`{"to_agent":"agent-b","subject":"S","body":"B","in_reply_to":"not-a-uuid"}`),
	postCase("mail send in_reply_to falsy", mailSendPath, "agent-a-session",
		`{"to_agent":"agent-b","subject":"S","body":"B","in_reply_to":0}`),
	postCase("mail send from_agent sanitized to the agent", mailSendPath, "agent-a-session",
		`{"to_agent":" Agent-B ","from_agent":" Agent-A ","subject":"S","body":"B"}`),
	postCase("mail send from_agent mismatch", mailSendPath, "agent-a-session",
		`{"to_agent":"agent-b","from_agent":"agent-b","subject":"S","body":"B"}`),
	postCase("mail send from_agent a number", mailSendPath, "agent-a-session",
		`{"to_agent":"agent-b","from_agent":5,"subject":"S","body":"B"}`),
	postCase("mail send from_agent sanitizes to nothing", mailSendPath, "agent-a-session",
		`{"to_agent":"agent-b","from_agent":"!!!","subject":"S","body":"B"}`),
	postCase("mail send with an API key", mailSendPath, "agent-b-key",
		`{"to_agent":"agent-a","subject":"Key","body":"Sent with a key."}`),
	postCase("mail send to an unknown agent", mailSendPath, "agent-a-session",
		`{"to_agent":"nobody-here","subject":"S","body":"B"}`),
	postCase("mail send missing subject", mailSendPath, "agent-a-session",
		`{"to_agent":"agent-b","body":"B"}`),
	postCase("mail send subject sanitized to nothing", mailSendPath, "agent-a-session",
		`{"to_agent":"agent-b","subject":"​\u0007","body":"B"}`),
	postCase("mail send body control characters stripped", mailSendPath, "agent-a-session",
		`{"to_agent":"agent-b","subject":"Clean","body":"a\u0000b‮c\td\ne"}`),
	postCase("mail send subject too long", mailSendPath, "agent-a-session",
		`{"to_agent":"agent-b","subject":"`+strings.Repeat("s", 501)+`","body":"B"}`),
	postCase("mail send as an admin for an agent", mailSendPath, "admin-web",
		`{"to_agent":"agent-a","from_agent":"agent-b","subject":"Admin","body":"On behalf."}`),
	postCase("mail send as an admin without from_agent", mailSendPath, "admin-web",
		`{"to_agent":"agent-a","subject":"S","body":"B"}`),
	postCase("mail send from system", mailSendPath, "admin-web",
		`{"to_agent":"agent-a","from_agent":"system","subject":"Welcome","body":"From the system."}`),
	postCase("mail send to_agent not a string", mailSendPath, "admin-web",
		`{"to_agent":5,"from_agent":"agent-b","subject":"S","body":"B"}`, notAStringInAdminSession("POST "+mailSendPath)...),
	// The notice comes back from the virtual agent in the background.
	{
		Name: "mail send to a virtual agent without access",
		Steps: []Step{
			{Method: "POST", Path: mailSendPath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"to_agent":"helper-va","subject":"Question","body":"Can you help?"}`},
			{Settle: true},
			{Method: "POST", Path: mailCheckPath, Auth: "agent-a-session", Header: jsonType, Body: `{}`},
		},
	},
	{
		Name: "mail send to a virtual agent with a Re: subject",
		Steps: []Step{
			{Method: "POST", Path: mailSendPath, Auth: "agent-b-session", Header: jsonType,
				Body: `{"to_agent":"helper-va","subject":"Re: From the helper","body":"Thanks."}`},
			{Settle: true},
		},
	},
}

var mailReadCases = []Scenario{
	postCase("mail check", mailCheckPath, "agent-a-session", `{}`),
	postCase("mail check agent sanitized", mailCheckPath, "agent-b-session", `{"agent":"AGENT-B"}`),
	postCase("mail check mismatch", mailCheckPath, "agent-a-session", `{"agent":"agent-b"}`),
	postCase("mail check as an admin", mailCheckPath, "admin-web", `{"agent":"agent-b"}`),
	postCase("mail check as an admin without agent", mailCheckPath, "admin-web", `{}`),
	postCase("mail check unknown agent", mailCheckPath, "admin-web", `{"agent":"nobody-here"}`),
	postCase("mail check an array body", mailCheckPath, "agent-a-session", `[]`),
	postCase("mail receive", mailReceivePath, "agent-a-session",
		`{"ids":["`+fixtureUnreadToA+`","`+strings.ToUpper(fixtureReadByA)+`","`+fixtureUnreadToB+`"]}`),
	postCase("mail receive without ids", mailReceivePath, "agent-a-session", `{}`),
	postCase("mail receive empty ids", mailReceivePath, "agent-a-session", `{"ids":[]}`),
	postCase("mail receive ids not an array", mailReceivePath, "agent-a-session", `{"ids":"`+fixtureUnreadToA+`"}`),
	postCase("mail receive a bad uuid", mailReceivePath, "agent-a-session", `{"ids":["`+fixtureUnreadToA+`","nope"]}`),
	postCase("mail receive a number id", mailReceivePath, "agent-a-session", `{"ids":[5]}`),
	postCase("mail receive mismatch", mailReceivePath, "agent-b-session", `{"agent":"agent-a","ids":["`+fixtureUnreadToA+`"]}`),
}

var mailAckCases = []Scenario{
	postCase("mail ack", mailAckPath, "agent-a-session", `{"ids":["`+fixtureUnreadToA+`","`+fixtureReadByA+`"]}`),
	postCase("mail ack legacy message_ids", mailAckPath, "agent-a-session", `{"message_ids":["`+fixtureUnreadToA+`"]}`),
	postCase("mail ack ids empty falls to message_ids", mailAckPath, "agent-a-session", `{"ids":"","message_ids":["`+fixtureUnreadToA+`"]}`),
	postCase("mail ack empty array", mailAckPath, "agent-a-session", `{"ids":[]}`),
	postCase("mail ack ids not an array", mailAckPath, "agent-a-session", `{"ids":"`+fixtureUnreadToA+`"}`),
	postCase("mail ack a non-uuid", mailAckPath, "agent-a-session", `{"ids":["nope"]}`),
	postCase("mail ack a number and a null", mailAckPath, "agent-a-session", `{"ids":["`+fixtureUnreadToA+`",5,null]}`),
	postCase("mail ack quotes in an id", mailAckPath, "agent-a-session", `{"ids":["a\"b\\c"]}`),
	postCase("mail ack someone else's mail", mailAckPath, "agent-b-session", `{"ids":["`+fixtureUnreadToA+`"]}`),
	postCase("mail ack mismatch", mailAckPath, "agent-b-session", `{"agent":"agent-a","ids":["`+fixtureUnreadToA+`"]}`),
}

var mailEditUnsendCases = []Scenario{
	postCase("mail edit subject", mailEditPath, "agent-a-session", `{"id":"`+fixtureUnreadToB+`","subject":"Edited subject"}`),
	postCase("mail edit both", mailEditPath, "agent-a-session", `{"id":"`+fixtureUnreadToB+`","subject":"S2","body":"B2"}`),
	postCase("mail edit by the recipient", mailEditPath, "agent-b-session", `{"id":"`+fixtureUnreadToB+`","body":"x"}`),
	postCase("mail edit an acked message", mailEditPath, "agent-a-session", `{"id":"`+fixtureReadByB+`","body":"x"}`),
	postCase("mail edit an unsent message", mailEditPath, "agent-a-session", `{"id":"`+fixtureUnsentToB+`","body":"x"}`),
	postCase("mail edit nothing to change", mailEditPath, "agent-a-session", `{"id":"`+fixtureUnreadToB+`"}`),
	postCase("mail edit missing id", mailEditPath, "agent-a-session", `{"body":"x"}`),
	postCase("mail edit id not a uuid", mailEditPath, "agent-a-session", `{"id":"nope","body":"x"}`),
	postCase("mail edit mismatch", mailEditPath, "agent-a-session", `{"id":"`+fixtureUnreadToB+`","from_agent":"agent-b","body":"x"}`),
	postCase("mail unsend", mailUnsendPath, "agent-a-session", `{"id":"`+fixtureUnreadToB+`"}`),
	postCase("mail unsend already unsent", mailUnsendPath, "agent-a-session", `{"id":"`+fixtureUnsentToB+`"}`),
	postCase("mail unsend by the recipient", mailUnsendPath, "agent-b-session", `{"id":"`+fixtureUnreadToB+`"}`),
	postCase("mail unsend missing id", mailUnsendPath, "agent-a-session", `{}`),
	postCase("mail unsend as an admin", mailUnsendPath, "admin-web", `{"id":"`+fixtureUnreadToB+`","from_agent":"agent-a"}`),
	{
		Name: "mail send, receive, edit, ack, then list",
		Steps: []Step{
			{Method: "POST", Path: mailSendPath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"to_agent":"agent-b","subject":"Round trip","body":"First version."}`, Capture: map[string]string{"mail": "id"}},
			{Method: "POST", Path: mailCheckPath, Auth: "agent-b-session", Header: jsonType, Body: `{}`},
			{Method: "POST", Path: mailEditPath, Auth: "agent-a-session", Header: jsonType, Body: `{"id":"{{mail}}","body":"Second version."}`},
			{Method: "POST", Path: mailReceivePath, Auth: "agent-b-session", Header: jsonType, Body: `{"ids":["{{mail}}"]}`},
			{Method: "POST", Path: mailAckPath, Auth: "agent-b-session", Header: jsonType, Body: `{"ids":["{{mail}}"]}`},
			{Method: "POST", Path: mailEditPath, Auth: "agent-a-session", Header: jsonType, Body: `{"id":"{{mail}}","body":"Too late."}`},
			{Method: "POST", Path: mailUnsendPath, Auth: "agent-a-session", Header: jsonType, Body: `{"id":"{{mail}}"}`},
			{Method: "POST", Path: mailHistoryPath, Auth: "agent-b-session", Header: jsonType, Body: `{"from":"agent-a","limit":2}`},
			{Method: "POST", Path: mailSentPath, Auth: "agent-a-session", Header: jsonType, Body: `{"to":"agent-b"}`},
		},
	},
}

var mailListCases = []Scenario{
	postCase("mail sent", mailSentPath, "agent-a-session", `{}`),
	postCase("mail sent to one recipient with limit and offset", mailSentPath, "agent-a-session", `{"to":"Agent-B","limit":1,"offset":"1"}`),
	postCase("mail sent limit as a string", mailSentPath, "agent-a-session", `{"limit":"1"}`),
	postCase("mail sent limit over the cap", mailSentPath, "agent-a-session", `{"limit":500}`),
	postCase("mail sent limit an empty array", mailSentPath, "agent-a-session", `{"limit":[]}`),
	postCase("mail sent limit not a number", mailSentPath, "agent-a-session", `{"limit":"abc"}`),
	postCase("mail sent limit negative", mailSentPath, "agent-a-session", `{"limit":-1}`),
	// Number() throws a TypeError for an object with its own toString key,
	// before the list query runs.
	postCase("mail sent limit an object Number cannot convert", mailSentPath, "agent-a-session", `{"limit":{"toString":null}}`),
	postCase("mail history limit an array Number cannot convert", mailHistoryPath, "agent-b-session", `{"limit":[{"toString":1}]}`),
	postCase("mail sent offset an array", mailSentPath, "agent-a-session", `{"offset":[1]}`),
	postCase("mail sent to an unknown agent", mailSentPath, "agent-a-session", `{"to":"nobody-here"}`),
	postCase("mail sent mismatch", mailSentPath, "agent-a-session", `{"agent":"agent-b"}`),
	postCase("mail history", mailHistoryPath, "agent-b-session", `{}`),
	postCase("mail history from one sender", mailHistoryPath, "agent-a-session", `{"from":"agent-b"}`),
	postCase("mail history limit a fraction", mailHistoryPath, "agent-a-session", `{"limit":2.5}`),
	postCase("mail history as an admin", mailHistoryPath, "admin-web", `{"agent":"agent-a","offset":1}`),
	postCase("mail history as an admin without agent", mailHistoryPath, "admin-web", `{}`),
}

// mailNoBody sends text/plain, so neither server parses a JSON body.
func mailNoBody(path string) Scenario {
	return one("mail "+path[len("/v1/mail/"):]+" without a JSON body", Step{Method: "POST", Path: path, Auth: "agent-a-session",
		Header: [][2]string{{"Content-Type", "text/plain"}}, Body: "x"})
}

var mailRoutingCases = []Scenario{
	mailNoBody(mailSendPath), mailNoBody(mailCheckPath), mailNoBody(mailReceivePath), mailNoBody(mailAckPath),
	mailNoBody(mailEditPath), mailNoBody(mailUnsendPath), mailNoBody(mailSentPath), mailNoBody(mailHistoryPath),
	one("mail send OPTIONS", Step{Method: "OPTIONS", Path: mailSendPath, Auth: "agent-a-session"}),
	one("mail check GET falls through", Step{Method: "GET", Path: mailCheckPath, Auth: "agent-a-session"}),
	one("mail send encoded path", Step{Method: "POST", Path: "/v1/%6Dail/send", Auth: "agent-a-session",
		Header: jsonType, Body: `{"to_agent":"agent-b","subject":"S","body":"B"}`}),
	one("mail check without credentials", Step{Method: "POST", Path: mailCheckPath, Header: jsonType, Body: `{}`}),
}
