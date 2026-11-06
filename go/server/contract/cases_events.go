package contract

// The admin WebSocket (/admin/ws, services/events.js): the upgrade, its
// refusals, the welcome message, and the events the ported routes broadcast.
var eventsCases = concat(wsHandshakeCases, wsRefusalCases, wsCloseCases, wsEventCases)

// wsKey is RFC 6455's sample key, so the Sec-WebSocket-Accept is fixed.
const wsKey = "dGhlIHNhbXBsZSBub25jZQ=="

// wsHeaders is a browser's upgrade request without the subprotocol header,
// with extra headers after it.
func wsHeaders(extra ...[2]string) [][2]string {
	h := [][2]string{
		{"Connection", "Upgrade"},
		{"Upgrade", "websocket"},
		{"Sec-WebSocket-Version", "13"},
		{"Sec-WebSocket-Key", wsKey},
	}
	return append(h, extra...)
}

// wsBearer is the subprotocol header the admin UI sends:
// new WebSocket(url, ['bearer', token]).
func wsBearer(token string) [2]string {
	return [2]string{"Sec-WebSocket-Protocol", "bearer, " + token}
}

// wsOpenStep opens connection conn as the admin, expecting the upgrade.
func wsOpenStep(conn string) Step {
	return Step{WS: &WSStep{Conn: conn, Open: true, Expect: "101", Header: wsHeaders(wsBearer("{{admin-web}}"))}}
}

func wsRead(conn string, n int) Step {
	return Step{WS: &WSStep{Conn: conn, Read: n}}
}

// wsUpgrades is a case whose request must be upgraded: the open, then the
// welcome message.
func wsUpgrades(name string, header [][2]string) Scenario {
	return Scenario{Name: name, Steps: []Step{
		{WS: &WSStep{Open: true, Expect: "101", Header: header}},
		wsRead("ws", 1),
	}}
}

// wsRefused is a case whose request gets expect ("401", "400", "405", or
// "closed" for no response at all).
func wsRefused(name, expect string, ws WSStep) Scenario {
	ws.Open = true
	ws.Expect = expect
	return one(name, Step{WS: &ws})
}

var wsHandshakeCases = []Scenario{
	{
		Name: "ws admin session connects, pings, closes",
		Steps: []Step{
			wsOpenStep("ws"),
			wsRead("ws", 1),
			{WS: &WSStep{Ping: true}},
			{WS: &WSStep{Close: true}},
		},
	},
	{
		// A request with the token first puts it in the session cache;
		// the upgrade then reads the cache (events.js) instead of the
		// database.
		Name: "ws admin session from the auth cache",
		Steps: []Step{
			{Method: "GET", Path: noRoutePath, Auth: "admin-web"},
			wsOpenStep("ws"),
			wsRead("ws", 1),
		},
	},
	wsUpgrades("ws version 8", [][2]string{
		{"Connection", "keep-alive, Upgrade"}, {"Upgrade", "WebSocket"},
		{"Sec-WebSocket-Version", "8"}, {"Sec-WebSocket-Key", wsKey}, wsBearer("{{admin-web}}"),
	}),
	wsUpgrades("ws first offered subprotocol is echoed", wsHeaders(
		[2]string{"Sec-WebSocket-Protocol", "x-first, bearer, {{admin-web}}"})),
	wsUpgrades("ws subprotocol header in two lines", wsHeaders(
		[2]string{"Sec-WebSocket-Protocol", "bearer"}, [2]string{"Sec-WebSocket-Protocol", "{{admin-web}}"})),
	wsUpgrades("ws version with spaces", [][2]string{
		{"Connection", "Upgrade"}, {"Upgrade", "websocket"},
		{"Sec-WebSocket-Version", " 13 "}, {"Sec-WebSocket-Key", wsKey}, wsBearer("{{admin-web}}"),
	}),
}

var wsRefusalCases = []Scenario{
	// Token refusals: a bare 401, then the socket is closed.
	wsRefused("ws no subprotocol header", "401", WSStep{Header: wsHeaders()}),
	wsRefused("ws bearer with no token", "401", WSStep{Header: wsHeaders([2]string{"Sec-WebSocket-Protocol", "bearer"})}),
	wsRefused("ws empty token", "401", WSStep{Header: wsHeaders([2]string{"Sec-WebSocket-Protocol", "bearer, , x"})}),
	wsRefused("ws token not first after bearer", "401", WSStep{Header: wsHeaders([2]string{"Sec-WebSocket-Protocol", "{{admin-web}}, bearer"})}),
	wsRefused("ws unknown token", "401", WSStep{Header: wsHeaders(wsBearer("not-a-session"))}),
	wsRefused("ws agent session is not an admin", "401", WSStep{Header: wsHeaders(wsBearer("{{agent-a-session}}"))}),
	wsRefused("ws api key is not an admin", "401", WSStep{Header: wsHeaders(wsBearer("{{agent-a-key}}"))}),
	wsRefused("ws Bearer is case-sensitive", "401", WSStep{Header: wsHeaders([2]string{"Sec-WebSocket-Protocol", "Bearer, {{admin-web}}"})}),
	// The token is checked before the handshake, so a bad handshake with
	// no token is still the 401.
	wsRefused("ws post without a token", "401", WSStep{Method: "POST", Header: wsHeaders()}),

	// Any other target has its socket destroyed with no response, the
	// query string counting, and nothing is request-logged.
	wsRefused("ws target with a query", "closed", WSStep{Target: "/admin/ws?x=1", Header: wsHeaders(wsBearer("{{admin-web}}"))}),
	wsRefused("ws target with a trailing slash", "closed", WSStep{Target: "/admin/ws/", Header: wsHeaders(wsBearer("{{admin-web}}"))}),
	wsRefused("ws upgrade on a v1 route", "closed", WSStep{Target: "/v1/agent/status", Header: wsHeaders(wsBearer("{{agent-a-session}}"))}),
	wsRefused("ws upgrade on health", "closed", WSStep{Target: "/health", Header: wsHeaders()}),
	wsRefused("ws h2c upgrade on a v1 route", "closed", WSStep{Target: "/v1/agent/status", Header: [][2]string{
		{"Connection", "Upgrade, HTTP2-Settings"}, {"Upgrade", "h2c"}, {"HTTP2-Settings", "AAMAAABkAARAAAAAAAIAAAAA"},
	}}),

	// ws's handshake checks, after a valid token.
	wsRefused("ws post", "405", WSStep{Method: "POST", Header: wsHeaders(wsBearer("{{admin-web}}"))}),
	wsRefused("ws h2c upgrade", "400", WSStep{Header: [][2]string{
		{"Connection", "Upgrade"}, {"Upgrade", "h2c"}, {"Sec-WebSocket-Version", "13"}, {"Sec-WebSocket-Key", wsKey}, wsBearer("{{admin-web}}"),
	}}),
	wsRefused("ws two upgrade values", "400", WSStep{Header: [][2]string{
		{"Connection", "Upgrade"}, {"Upgrade", "websocket, h2c"}, {"Sec-WebSocket-Version", "13"}, {"Sec-WebSocket-Key", wsKey}, wsBearer("{{admin-web}}"),
	}}),
	wsRefused("ws missing key", "400", WSStep{Header: [][2]string{
		{"Connection", "Upgrade"}, {"Upgrade", "websocket"}, {"Sec-WebSocket-Version", "13"}, wsBearer("{{admin-web}}"),
	}}),
	wsRefused("ws short key", "400", WSStep{Header: [][2]string{
		{"Connection", "Upgrade"}, {"Upgrade", "websocket"}, {"Sec-WebSocket-Version", "13"}, {"Sec-WebSocket-Key", "c2hvcnQ="}, wsBearer("{{admin-web}}"),
	}}),
	wsRefused("ws key in two lines", "400", WSStep{Header: wsHeaders(wsBearer("{{admin-web}}"), [2]string{"Sec-WebSocket-Key", wsKey})}),
	wsRefused("ws missing version", "400", WSStep{Header: [][2]string{
		{"Connection", "Upgrade"}, {"Upgrade", "websocket"}, {"Sec-WebSocket-Key", wsKey}, wsBearer("{{admin-web}}"),
	}}),
	wsRefused("ws version 12", "400", WSStep{Header: [][2]string{
		{"Connection", "Upgrade"}, {"Upgrade", "websocket"}, {"Sec-WebSocket-Version", "12"}, {"Sec-WebSocket-Key", wsKey}, wsBearer("{{admin-web}}"),
	}}),
	wsRefused("ws subprotocol trailing comma", "400", WSStep{Header: wsHeaders([2]string{"Sec-WebSocket-Protocol", "bearer, {{admin-web}},"})}),
	wsRefused("ws subprotocol repeated", "400", WSStep{Header: wsHeaders([2]string{"Sec-WebSocket-Protocol", "bearer, {{admin-web}}, bearer"})}),
	wsRefused("ws subprotocol with a bad character", "400", WSStep{Header: wsHeaders([2]string{"Sec-WebSocket-Protocol", "bearer, {{admin-web}}, a/b"})}),
}

// wsCloseCases close from the client: the server echoes the code and the
// reason, or an empty payload when the close had no status.
var wsCloseCases = []Scenario{
	{Name: "ws close with a reason", Steps: []Step{
		wsOpenStep("ws"), wsRead("ws", 1),
		{WS: &WSStep{Close: true, CloseCode: 1000, CloseReason: "done"}},
	}},
	{Name: "ws close with an application code", Steps: []Step{
		wsOpenStep("ws"), wsRead("ws", 1),
		{WS: &WSStep{Close: true, CloseCode: 4000, CloseReason: "going away now"}},
	}},
	{Name: "ws close with no status", Steps: []Step{
		wsOpenStep("ws"), wsRead("ws", 1),
		{WS: &WSStep{Close: true, CloseCode: -1}},
	}},
}

// wsEventCases connect a dashboard and then make the calls that broadcast.
var wsEventCases = []Scenario{
	{
		Name: "ws agent activity events",
		Steps: []Step{
			wsOpenStep("ws"),
			wsRead("ws", 1),
			{Method: "POST", Path: agentActivityStart, Auth: "agent-a-session", Header: jsonType, Body: `{}`},
			wsRead("ws", 1),
			{Method: "POST", Path: agentActivityStop, Auth: "agent-a-session", Header: jsonType, Body: `{}`},
			wsRead("ws", 1),
		},
	},
	{
		Name: "ws note events",
		Steps: []Step{
			wsOpenStep("ws"),
			wsRead("ws", 1),
			{Method: "POST", Path: savePath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"agent-a","title":"Live","content":"## Live\nWatched.","slug":"notes/live"}`},
			wsRead("ws", 1),
			// Each write indexes or drops chunks in the background, and the
			// next step changes the same rows.
			{Settle: true},
			{Method: "POST", Path: editPath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"agent-a","slug":"notes/live","old_string":"Watched","new_string":"Seen"}`},
			wsRead("ws", 1),
			{Settle: true},
			{Method: "POST", Path: movePath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"agent-a","slug":"notes/live","new_slug":"notes/moved","new_namespace":"shared"}`},
			wsRead("ws", 2),
			{Method: "POST", Path: docDelPath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"shared","slug":"notes/moved"}`},
			wsRead("ws", 1),
			{Settle: true},
			{Method: "POST", Path: restorePath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"shared","slug":"notes/moved"}`},
			wsRead("ws", 1),
			{Settle: true},
		},
	},
	{
		Name: "ws chat event",
		Steps: []Step{
			wsOpenStep("ws"),
			wsRead("ws", 1),
			{Method: "POST", Path: chatSendPath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"to_agents":["agent-b"],"message":"hello <dashboard> & \"you\" é"}`},
			wsRead("ws", 1),
		},
	},
	{
		// Two dashboards each get every event, in order, after their own
		// welcome.
		Name: "ws two dashboards",
		Steps: []Step{
			wsOpenStep("a"),
			wsRead("a", 1),
			wsOpenStep("b"),
			wsRead("b", 1),
			{Method: "POST", Path: agentActivityStart, Auth: "agent-a-session", Header: jsonType, Body: `{}`},
			{Method: "POST", Path: agentActivityStop, Auth: "agent-b-session", Header: jsonType, Body: `{}`},
			wsRead("a", 2),
			wsRead("b", 2),
			{WS: &WSStep{Conn: "a", Close: true}},
			{Method: "POST", Path: agentActivityStart, Auth: "agent-b-session", Header: jsonType, Body: `{}`},
			wsRead("b", 1),
		},
	},
}
