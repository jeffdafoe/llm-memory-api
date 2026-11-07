package contract

// systemCases cover POST /v1/system/error/report (routes/system.js and
// services/error-handler.js): the system_errors row, the SUBAGENT_DEAD
// handler's system message (chat_message_texts and chat_messages), the
// handler failures, and the refusals.
var systemCases = concat(systemReportCases, systemSubagentDeadCases, systemRefusalCases)

const systemReportPath = "/v1/system/error/report"

func systemReport(name, auth, body string) Scenario {
	return postCase(name, systemReportPath, auth, body)
}

var systemReportCases = []Scenario{
	systemReport("system error report unhandled", "agent-a-session",
		`{"source":"memory-sync","error_code":"UPLOAD_FAILED","context":{"b":2,"a":{"x":[1,"two",null]},"2":true,"1":1e400,"n":12345678901234567890}}`),
	systemReport("system error report without context", "agent-b-key",
		`{"source":"memory-sync","error_code":"UPLOAD_FAILED"}`),
	systemReport("system error report empty context", "agent-a-session",
		`{"source":"x","error_code":"Y","context":{}}`),
	// Same source as the handler, another code: no handler.
	systemReport("system error report other discuss-transport code", "agent-a-session",
		`{"source":"discuss-transport","error_code":"SUBAGENT_SLOW","context":{"discussion_id":7}}`),
	// The handler key needs both parts exact.
	systemReport("system error report handler key is case-sensitive", "agent-a-session",
		`{"source":"discuss-transport","error_code":"subagent_dead"}`),
	{
		Name: "system error report twice",
		Steps: []Step{
			{Method: "POST", Path: systemReportPath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"source":"a","error_code":"B"}`},
			{Method: "POST", Path: systemReportPath, Auth: "agent-b-session", Header: jsonType,
				Body: `{"source":"discuss-transport","error_code":"SUBAGENT_DEAD","context":{"discussion_id":3}}`},
		},
	},
	// A web-user session has no agent: no actor on the row.
	systemReport("system error report as an admin", "admin-web",
		`{"source":"admin-ui","error_code":"RENDER_FAILED","context":{"page":"notes"}}`),
	// jsonb refuses \u0000: the insert fails after the checks, a 500.
	systemReport("system error report context jsonb refuses nul", "agent-a-session",
		`{"source":"x","error_code":"Y","context":{"s":"a\u0000b"}}`),
}

var systemSubagentDeadCases = []Scenario{
	systemReport("system subagent dead", "agent-a-session",
		`{"source":"discuss-transport","error_code":"SUBAGENT_DEAD","context":{"discussion_id":42,"agent":"agent-a","messages_sent":3,"elapsed_minutes":10,"silent_minutes":4,"last_outbox_activity":"2026-11-07T10:00:00Z","last_inbox_delivery":"2026-11-07T10:01:00Z"}}`),
	systemReport("system subagent dead with an API key", "agent-b-key",
		`{"source":"discuss-transport","error_code":"SUBAGENT_DEAD","context":{"discussion_id":9}}`),
	systemReport("system subagent dead without context", "agent-a-session",
		`{"source":"discuss-transport","error_code":"SUBAGENT_DEAD"}`),
	systemReport("system subagent dead discussion_id 0", "agent-a-session",
		`{"source":"discuss-transport","error_code":"SUBAGENT_DEAD","context":{"discussion_id":0}}`),
	systemReport("system subagent dead discussion_id empty string", "agent-a-session",
		`{"source":"discuss-transport","error_code":"SUBAGENT_DEAD","context":{"discussion_id":""}}`),
	// The id goes into the message as String() writes it.
	systemReport("system subagent dead discussion_id a string", "agent-a-session",
		`{"source":"discuss-transport","error_code":"SUBAGENT_DEAD","context":{"discussion_id":"12 <b>"}}`),
	systemReport("system subagent dead discussion_id a float", "agent-a-session",
		`{"source":"discuss-transport","error_code":"SUBAGENT_DEAD","context":{"discussion_id":1.50e21}}`),
	systemReport("system subagent dead discussion_id an array", "agent-a-session",
		`{"source":"discuss-transport","error_code":"SUBAGENT_DEAD","context":{"discussion_id":[1,null,"x",[2,3]]}}`),
	systemReport("system subagent dead discussion_id an object", "agent-a-session",
		`{"source":"discuss-transport","error_code":"SUBAGENT_DEAD","context":{"discussion_id":{"a":1}}}`),
	systemReport("system subagent dead discussion_id true", "agent-a-session",
		`{"source":"discuss-transport","error_code":"SUBAGENT_DEAD","context":{"discussion_id":true}}`),
	// String() of an object with its own toString member throws: the
	// handler fails, no message, the row is unhandled.
	systemReport("system subagent dead discussion_id not convertible", "agent-a-session",
		`{"source":"discuss-transport","error_code":"SUBAGENT_DEAD","context":{"discussion_id":{"toString":1}}}`),
	// No agent to message: the handler fails.
	systemReport("system subagent dead as an admin", "admin-web",
		`{"source":"discuss-transport","error_code":"SUBAGENT_DEAD","context":{"discussion_id":5}}`),
	// The message is sent before the insert fails.
	systemReport("system subagent dead context jsonb refuses nul", "agent-a-session",
		`{"source":"discuss-transport","error_code":"SUBAGENT_DEAD","context":{"discussion_id":5,"s":"\u0000"}}`),
}

var systemRefusalCases = []Scenario{
	one("system error report no body", Step{Method: "POST", Path: systemReportPath, Auth: "agent-a-session"}),
	systemReport("system error report empty object", "agent-a-session", `{}`),
	systemReport("system error report missing error_code", "agent-a-session", `{"source":"x"}`),
	systemReport("system error report missing source", "agent-a-session", `{"error_code":"Y"}`),
	systemReport("system error report empty source", "agent-a-session", `{"source":"","error_code":"Y"}`),
	systemReport("system error report source 0", "agent-a-session", `{"source":0,"error_code":"Y"}`),
	systemReport("system error report source a number", "agent-a-session", `{"source":5,"error_code":"Y"}`),
	systemReport("system error report error_code an object", "agent-a-session", `{"source":"x","error_code":{}}`),
	systemReport("system error report error_code an array", "agent-a-session", `{"source":"x","error_code":["Y"]}`),
	systemReport("system error report context null", "agent-a-session", `{"source":"x","error_code":"Y","context":null}`),
	systemReport("system error report context an array", "agent-a-session", `{"source":"x","error_code":"Y","context":[1]}`),
	systemReport("system error report context a string", "agent-a-session", `{"source":"x","error_code":"Y","context":"z"}`),
	systemReport("system error report context false", "agent-a-session", `{"source":"x","error_code":"Y","context":false}`),
	systemReport("system error report context 0", "agent-a-session", `{"source":"x","error_code":"Y","context":0}`),
	// The required-fields check runs before the context check.
	systemReport("system error report bad context and no source", "agent-a-session", `{"error_code":"Y","context":null}`),
	systemReport("system error report body an array", "agent-a-session", `[{"source":"x","error_code":"Y"}]`),
	one("system error report form body", Step{Method: "POST", Path: systemReportPath, Auth: "agent-a-session",
		Header: [][2]string{{"Content-Type", "application/x-www-form-urlencoded"}}, Body: "source=x&error_code=Y"}),
	one("system error report without auth", Step{Method: "POST", Path: systemReportPath, Header: jsonType,
		Body: `{"source":"x","error_code":"Y"}`}),
	systemReport("system error report bad token", "agent-b-revoked-key", `{"source":"x","error_code":"Y"}`),
	systemReport("system error report expired session", "agent-b-expired-session", `{"source":"x","error_code":"Y"}`),
	one("system error report GET", Step{Method: "GET", Path: systemReportPath, Auth: "agent-a-session"}),
}
