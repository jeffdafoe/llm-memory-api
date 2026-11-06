package contract

// The admin WebSocket (/admin/ws, services/events.js): the upgrade, its
// refusals, the welcome message, and the events the ported routes broadcast.
var eventsCases = concat(wsHandshakeCases, wsRefusalCases, wsEventCases)

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

// wsOpenStep opens connection conn with the given headers.
func wsOpenStep(conn string, header [][2]string) Step {
	return Step{WS: &WSStep{Conn: conn, Open: true, Header: header}}
}

func wsRead(conn string, n int) Step {
	return Step{WS: &WSStep{Conn: conn, Read: n}}
}

func wsOne(name string, ws WSStep) Scenario {
	return one(name, Step{WS: &ws})
}

var wsHandshakeCases = []Scenario{
	{
		Name: "ws admin session connects, pings, closes",
		Steps: []Step{
			wsOpenStep("ws", wsHeaders(wsBearer("{{admin-web}}"))),
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
			wsOpenStep("ws", wsHeaders(wsBearer("{{admin-web}}"))),
			wsRead("ws", 1),
		},
	},
	wsOne("ws version 8", WSStep{Open: true, Header: [][2]string{
		{"Connection", "keep-alive, Upgrade"}, {"Upgrade", "WebSocket"},
		{"Sec-WebSocket-Version", "8"}, {"Sec-WebSocket-Key", wsKey}, wsBearer("{{admin-web}}"),
	}}),
	wsOne("ws first offered subprotocol is echoed", WSStep{Open: true, Header: wsHeaders(
		[2]string{"Sec-WebSocket-Protocol", "x-first, bearer, {{admin-web}}"})}),
	wsOne("ws subprotocol header in two lines", WSStep{Open: true, Header: wsHeaders(
		[2]string{"Sec-WebSocket-Protocol", "bearer"}, [2]string{"Sec-WebSocket-Protocol", "{{admin-web}}"})}),
	wsOne("ws version with spaces", WSStep{Open: true, Header: [][2]string{
		{"Connection", "Upgrade"}, {"Upgrade", "websocket"},
		{"Sec-WebSocket-Version", " 13 "}, {"Sec-WebSocket-Key", wsKey}, wsBearer("{{admin-web}}"),
	}}),
}

var wsRefusalCases = []Scenario{
	// Token refusals: a bare 401, then the socket is closed.
	wsOne("ws no subprotocol header", WSStep{Open: true, Header: wsHeaders()}),
	wsOne("ws bearer with no token", WSStep{Open: true, Header: wsHeaders([2]string{"Sec-WebSocket-Protocol", "bearer"})}),
	wsOne("ws empty token", WSStep{Open: true, Header: wsHeaders([2]string{"Sec-WebSocket-Protocol", "bearer, , x"})}),
	wsOne("ws token not first after bearer", WSStep{Open: true, Header: wsHeaders([2]string{"Sec-WebSocket-Protocol", "{{admin-web}}, bearer"})}),
	wsOne("ws unknown token", WSStep{Open: true, Header: wsHeaders(wsBearer("not-a-session"))}),
	wsOne("ws agent session is not an admin", WSStep{Open: true, Header: wsHeaders(wsBearer("{{agent-a-session}}"))}),
	wsOne("ws api key is not an admin", WSStep{Open: true, Header: wsHeaders(wsBearer("{{agent-a-key}}"))}),
	wsOne("ws Bearer is case-sensitive", WSStep{Open: true, Header: wsHeaders([2]string{"Sec-WebSocket-Protocol", "Bearer, {{admin-web}}"})}),
	// The token is checked before the handshake, so a bad handshake with
	// no token is still the 401.
	wsOne("ws post without a token", WSStep{Open: true, Method: "POST", Header: wsHeaders()}),

	// Any other target has its socket destroyed with no response, the
	// query string counting, and nothing is request-logged.
	wsOne("ws target with a query", WSStep{Open: true, Target: "/admin/ws?x=1", Header: wsHeaders(wsBearer("{{admin-web}}"))}),
	wsOne("ws target with a trailing slash", WSStep{Open: true, Target: "/admin/ws/", Header: wsHeaders(wsBearer("{{admin-web}}"))}),
	wsOne("ws upgrade on a v1 route", WSStep{Open: true, Target: "/v1/agent/status", Header: wsHeaders(wsBearer("{{agent-a-session}}"))}),
	wsOne("ws upgrade on health", WSStep{Open: true, Target: "/health", Header: wsHeaders()}),
	wsOne("ws h2c upgrade on a v1 route", WSStep{Open: true, Target: "/v1/agent/status", Header: [][2]string{
		{"Connection", "Upgrade, HTTP2-Settings"}, {"Upgrade", "h2c"}, {"HTTP2-Settings", "AAMAAABkAARAAAAAAAIAAAAA"},
	}}),

	// ws's handshake checks, after a valid token.
	wsOne("ws post", WSStep{Open: true, Method: "POST", Header: wsHeaders(wsBearer("{{admin-web}}"))}),
	wsOne("ws h2c upgrade", WSStep{Open: true, Header: [][2]string{
		{"Connection", "Upgrade"}, {"Upgrade", "h2c"}, {"Sec-WebSocket-Version", "13"}, {"Sec-WebSocket-Key", wsKey}, wsBearer("{{admin-web}}"),
	}}),
	wsOne("ws two upgrade values", WSStep{Open: true, Header: [][2]string{
		{"Connection", "Upgrade"}, {"Upgrade", "websocket, h2c"}, {"Sec-WebSocket-Version", "13"}, {"Sec-WebSocket-Key", wsKey}, wsBearer("{{admin-web}}"),
	}}),
	wsOne("ws missing key", WSStep{Open: true, Header: [][2]string{
		{"Connection", "Upgrade"}, {"Upgrade", "websocket"}, {"Sec-WebSocket-Version", "13"}, wsBearer("{{admin-web}}"),
	}}),
	wsOne("ws short key", WSStep{Open: true, Header: [][2]string{
		{"Connection", "Upgrade"}, {"Upgrade", "websocket"}, {"Sec-WebSocket-Version", "13"}, {"Sec-WebSocket-Key", "c2hvcnQ="}, wsBearer("{{admin-web}}"),
	}}),
	wsOne("ws key in two lines", WSStep{Open: true, Header: wsHeaders(wsBearer("{{admin-web}}"), [2]string{"Sec-WebSocket-Key", wsKey})}),
	wsOne("ws missing version", WSStep{Open: true, Header: [][2]string{
		{"Connection", "Upgrade"}, {"Upgrade", "websocket"}, {"Sec-WebSocket-Key", wsKey}, wsBearer("{{admin-web}}"),
	}}),
	wsOne("ws version 12", WSStep{Open: true, Header: [][2]string{
		{"Connection", "Upgrade"}, {"Upgrade", "websocket"}, {"Sec-WebSocket-Version", "12"}, {"Sec-WebSocket-Key", wsKey}, wsBearer("{{admin-web}}"),
	}}),
	wsOne("ws subprotocol trailing comma", WSStep{Open: true, Header: wsHeaders([2]string{"Sec-WebSocket-Protocol", "bearer, {{admin-web}},"})}),
	wsOne("ws subprotocol repeated", WSStep{Open: true, Header: wsHeaders([2]string{"Sec-WebSocket-Protocol", "bearer, {{admin-web}}, bearer"})}),
	wsOne("ws subprotocol with a bad character", WSStep{Open: true, Header: wsHeaders([2]string{"Sec-WebSocket-Protocol", "bearer, {{admin-web}}, a/b"})}),
}

// wsEventCases connect a dashboard and then make the calls that broadcast.
var wsEventCases = []Scenario{
	{
		Name: "ws agent activity events",
		Steps: []Step{
			wsOpenStep("ws", wsHeaders(wsBearer("{{admin-web}}"))),
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
			wsOpenStep("ws", wsHeaders(wsBearer("{{admin-web}}"))),
			wsRead("ws", 1),
			{Method: "POST", Path: savePath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"agent-a","title":"Live","content":"## Live\nWatched.","slug":"notes/live"}`},
			wsRead("ws", 1),
			{Method: "POST", Path: editPath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"agent-a","slug":"notes/live","old_string":"Watched","new_string":"Seen"}`},
			wsRead("ws", 1),
			{Method: "POST", Path: movePath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"agent-a","slug":"notes/live","new_slug":"notes/moved","new_namespace":"shared"}`},
			wsRead("ws", 2),
			{Method: "POST", Path: docDelPath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"shared","slug":"notes/moved"}`},
			wsRead("ws", 1),
			{Method: "POST", Path: restorePath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"namespace":"shared","slug":"notes/moved"}`},
			wsRead("ws", 1),
			{Settle: true},
		},
	},
	{
		Name: "ws chat event",
		Steps: []Step{
			wsOpenStep("ws", wsHeaders(wsBearer("{{admin-web}}"))),
			wsRead("ws", 1),
			{Method: "POST", Path: chatSendPath, Auth: "agent-a-session", Header: jsonType,
				Body: `{"to_agents":["agent-b"],"message":"hello dashboard"}`},
			wsRead("ws", 1),
		},
	},
	{
		// Two dashboards each get every event, in order, after their own
		// welcome.
		Name: "ws two dashboards",
		Steps: []Step{
			wsOpenStep("a", wsHeaders(wsBearer("{{admin-web}}"))),
			wsRead("a", 1),
			wsOpenStep("b", wsHeaders(wsBearer("{{admin-web}}"))),
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
