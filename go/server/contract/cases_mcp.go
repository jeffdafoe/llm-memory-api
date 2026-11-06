package contract

import (
	"fmt"
	"strings"
)

// mcpCases cover /mcp: the protocol layer for both eras (the 2025
// initialize handshake answered as an SSE stream, the 2026-07-28 per-request
// envelope answered as JSON), the auth in front of it, and the tools. The
// MCP client cases drive the real TypeScript SDK client against each server.
const mcpPath = "/mcp"

// legacyHeaders are what a 2025-era client sends.
var legacyHeaders = [][2]string{{"Content-Type", "application/json"}, {"Accept", "application/json, text/event-stream"}}

func mcpStep(auth, body string, extra ...[2]string) Step {
	h := append(append([][2]string{}, legacyHeaders...), extra...)
	return Step{Method: "POST", Path: mcpPath, Auth: auth, Header: h, Body: body}
}

func mcpCase(name, auth, body string, extra ...[2]string) Scenario {
	return one(name, mcpStep(auth, body, extra...))
}

// rpc is one JSON-RPC request body.
func rpc(id int, method, params string) string {
	if params == "" {
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q}`, id, method)
	}
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, params)
}

// call is a 2025-era tools/call.
func call(id int, tool, args string) string {
	if args == "" {
		return rpc(id, "tools/call", fmt.Sprintf(`{"name":%q}`, tool))
	}
	return rpc(id, "tools/call", fmt.Sprintf(`{"name":%q,"arguments":%s}`, tool, args))
}

func toolCase(name, auth, tool, args string) Scenario {
	return mcpCase(name, auth, call(1, tool, args))
}

// grantAgentB gives agent-b the note tools' permissions (the fixture gives
// it none), read on each request as mcpAuth reads them.
var grantAgentB = Step{SQL: `INSERT INTO agent_permissions (actor_id, permission_id)
    SELECT a.id, p.id FROM actors a, permissions p
    WHERE a.name = 'agent-b' AND p.name IN ('mcp_search', 'mcp_list_notes', 'mcp_read_note', 'mcp_save_note')`}

// agentBToolCase is a tool call by agent-b holding the note tools.
func agentBToolCase(name, tool, args string) Scenario {
	return Scenario{Name: name, Steps: []Step{grantAgentB, mcpStep("agent-b-mcp", call(1, tool, args))}}
}

// modernMeta is a valid 2026-07-28 request envelope.
const modernMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"contract","version":"1.0"}}`

// modern is a 2026-07-28 request with its standard headers.
func modern(id int, method, params string, extra ...[2]string) Step {
	p := "{" + modernMeta + "}"
	if params != "" {
		p = "{" + params + "," + modernMeta + "}"
	}
	h := [][2]string{{"Content-Type", "application/json"}, {"MCP-Protocol-Version", "2026-07-28"}, {"Mcp-Method", method}}
	return Step{Method: "POST", Path: mcpPath, Auth: "agent-a-mcp", Header: append(h, extra...),
		Body: fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, id, method, p)}
}

var mcpCases = concat(mcpProtocolCases, mcpModernCases, mcpToolCases, mcpNoteCases, mcpClientCases)

var mcpProtocolCases = []Scenario{
	one("mcp HEAD needs no auth", Step{Method: "HEAD", Path: mcpPath}),
	one("mcp without credentials", Step{Method: "POST", Path: mcpPath, Header: legacyHeaders, Body: rpc(1, "initialize", "{}")}),
	one("mcp forged bearer", mcpStep("", rpc(1, "ping", ""), [2]string{"Authorization", "Bearer agent-a:" + strings.Repeat("0", 64)})),
	one("mcp unknown API key", mcpStep("", rpc(1, "ping", ""), [2]string{"Authorization", "Bearer " + strings.Repeat("ab", 32)})),
	one("mcp token in the query", Step{Method: "POST", Path: mcpPath + "?token={{agent-a-mcp}}", Header: legacyHeaders, Body: rpc(1, "ping", "")}),
	one("mcp repeated token in the query", Step{Method: "POST", Path: mcpPath + "?token={{agent-a-mcp}}&token=x", Header: legacyHeaders, Body: rpc(1, "ping", "")}),
	mcpCase("mcp API key", "agent-a-key", rpc(1, "ping", "")),
	one("mcp GET", Step{Method: "GET", Path: mcpPath, Auth: "agent-a-mcp", Header: [][2]string{{"Accept", "text/event-stream"}}}),
	one("mcp DELETE", Step{Method: "DELETE", Path: mcpPath, Auth: "agent-a-mcp", Header: [][2]string{{"Mcp-Session-Id", "any"}}}),
	one("mcp OPTIONS", Step{Method: "OPTIONS", Path: mcpPath, Auth: "agent-a-mcp"}),
	one("mcp text/plain", Step{Method: "POST", Path: mcpPath, Auth: "agent-a-mcp", Header: [][2]string{{"Content-Type", "text/plain"}}, Body: rpc(1, "ping", "")}),
	one("mcp without a content type", Step{Method: "POST", Path: mcpPath, Auth: "agent-a-mcp", Body: rpc(1, "ping", "")}),
	one("mcp content type with a quoted comma", Step{Method: "POST", Path: mcpPath, Auth: "agent-a-mcp",
		Header: [][2]string{{"Content-Type", `application/json; profile="a,b"`}, {"Accept", "application/json, text/event-stream"}}, Body: rpc(1, "ping", "")}),
	one("mcp joined content types", Step{Method: "POST", Path: mcpPath, Auth: "agent-a-mcp",
		Header: [][2]string{{"Content-Type", "application/json, text/plain"}, {"Accept", "application/json, text/event-stream"}}, Body: rpc(1, "ping", "")}),
	one("mcp content type with a bad parameter", Step{Method: "POST", Path: mcpPath, Auth: "agent-a-mcp",
		Header: [][2]string{{"Content-Type", "application/json; a"}, {"Accept", "application/json, text/event-stream"}}, Body: rpc(1, "ping", "")}),
	one("mcp +json content type", Step{Method: "POST", Path: mcpPath, Auth: "agent-a-mcp", Header: [][2]string{{"Content-Type", "application/vnd.api+json"}}, Body: "{}"}),
	one("mcp malformed JSON", Step{Method: "POST", Path: mcpPath, Auth: "agent-a-mcp", Header: legacyHeaders, Body: `{bad`},
		KnownDifference{Where: "step 1 POST /mcp: body.error.message", Reason: "The text of a malformed-JSON 400 is Go's, not V8's (go-server note, Known differences)"}),
	one("mcp without Accept", Step{Method: "POST", Path: mcpPath, Auth: "agent-a-mcp", Header: [][2]string{{"Content-Type", "application/json"}}, Body: rpc(1, "ping", "")}),
	one("mcp Accept JSON only", Step{Method: "POST", Path: mcpPath, Auth: "agent-a-mcp", Header: [][2]string{{"Content-Type", "application/json"}, {"Accept", "application/json"}}, Body: rpc(1, "ping", "")}),
	mcpCase("mcp initialize", "agent-a-mcp", rpc(1, "initialize", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"contract","version":"1.0"}}`),
		[2]string{"User-Agent", "contract-harness"}, [2]string{"X-Forwarded-For", "10.0.0.1, 203.0.113.9"}),
	mcpCase("mcp initialize with an unknown version", "agent-a-mcp", rpc(2, "initialize", `{"protocolVersion":"1999-01-01","capabilities":{},"clientInfo":{"name":"c","version":"1"}}`)),
	mcpCase("mcp initialize with a modern header", "agent-a-mcp", rpc(1, "initialize", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}`),
		[2]string{"MCP-Protocol-Version", "2026-07-28"}),
	mcpCase("mcp initialize without params", "agent-a-mcp", rpc(1, "initialize", "")),
	mcpCase("mcp initialize with bad params", "agent-a-mcp", rpc(1, "initialize", `{"protocolVersion":5,"clientInfo":{"name":"c"}}`)),
	mcpCase("mcp initialized notification", "agent-a-mcp", `{"jsonrpc":"2.0","method":"notifications/initialized"}`),
	mcpCase("mcp ping", "agent-a-mcp", rpc(7, "ping", `{"x":1}`), [2]string{"MCP-Protocol-Version", "2025-11-25"}),
	mcpCase("mcp string id", "agent-a-mcp", `{"jsonrpc":"2.0","id":"abc","method":"ping"}`),
	mcpCase("mcp unsupported legacy header", "agent-a-mcp", rpc(3, "tools/list", ""), [2]string{"MCP-Protocol-Version", "2024-01-01"}),
	mcpCase("mcp modern header without a claim", "agent-a-mcp", rpc(3, "tools/list", ""), [2]string{"MCP-Protocol-Version", "2026-07-28"}),
	mcpCase("mcp modern header with a partial envelope", "agent-a-mcp", rpc(3, "tools/list", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}`),
		[2]string{"MCP-Protocol-Version", "foo"}),
	mcpCase("mcp unknown method", "agent-a-mcp", rpc(5, "foo/bar", "")),
	mcpCase("mcp claim-less server/discover", "agent-a-mcp", rpc(8, "server/discover", "")),
	mcpCase("mcp resources/list", "agent-a-mcp", rpc(5, "resources/list", "")),
	mcpCase("mcp empty object", "agent-a-mcp", `{}`),
	mcpCase("mcp empty body", "agent-a-mcp", ``),
	mcpCase("mcp null id", "agent-a-mcp", `{"jsonrpc":"2.0","id":null,"method":"ping"}`),
	mcpCase("mcp fractional id", "agent-a-mcp", `{"jsonrpc":"2.0","id":1.5,"method":"ping"}`),
	mcpCase("mcp huge id", "agent-a-mcp", `{"jsonrpc":"2.0","id":12345678901234567890,"method":"ping"}`),
	mcpCase("mcp extra key", "agent-a-mcp", `{"jsonrpc":"2.0","id":9,"method":"ping","x":1}`),
	mcpCase("mcp wrong jsonrpc", "agent-a-mcp", `{"jsonrpc":"1.0","id":1,"method":"ping"}`),
	mcpCase("mcp params array", "agent-a-mcp", `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":[1]}`),
	mcpCase("mcp empty batch", "agent-a-mcp", `[]`),
	mcpCase("mcp batch", "agent-a-mcp", "["+rpc(1, "ping", "")+","+rpc(2, "foo/bar", "")+"]"),
	mcpCase("mcp batch with a notification", "agent-a-mcp", "["+rpc(1, "ping", "")+`,{"jsonrpc":"2.0","method":"notifications/x"}]`),
	mcpCase("mcp batch of notifications", "agent-a-mcp", `[{"jsonrpc":"2.0","method":"a"},{"jsonrpc":"2.0","method":"b"}]`),
	mcpCase("mcp batch with duplicate ids", "agent-a-mcp", "["+rpc(1, "ping", "")+","+rpc(1, "foo/bar", "")+"]"),
	mcpCase("mcp batch with duplicate in-memory ids", "agent-a-mcp", "["+rpc(1, "ping", "")+","+rpc(1, "tools/list", "")+"]"),
	// -0 and 0 are one id, as a Map key compares them.
	mcpCase("mcp batch with ids -0 and 0", "agent-a-mcp", `[{"jsonrpc":"2.0","id":-0,"method":"ping"},{"jsonrpc":"2.0","id":0,"method":"tools/list"}]`),
	mcpCase("mcp batch with a tool call sharing id 0", "agent-a-mcp", `[{"jsonrpc":"2.0","id":0,"method":"tools/call","params":{"name":"read_instructions"}},{"jsonrpc":"2.0","id":-0,"method":"ping"}]`),
	// Node answers an unhandled method during its pass over the batch, the
	// in-memory methods next, the tool call last.
	mcpCase("mcp batch answer order", "agent-a-mcp", "["+call(1, "read_note", `{"slug":"instructions/deploy"}`)+","+rpc(2, "ping", "")+","+rpc(3, "foo/bar", "")+"]"),
	mcpCase("mcp batch with an invalid message", "agent-a-mcp", "["+rpc(1, "ping", "")+`,{"x":1}]`),
	mcpCase("mcp batch with a modern request", "agent-a-mcp", "["+rpc(1, "ping", `{`+modernMeta+`}`)+"]"),
	mcpCase("mcp batch with initialize", "agent-a-mcp", "["+rpc(1, "initialize", `{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}`)+","+rpc(2, "ping", "")+"]"),
	mcpCase("mcp batch over 100", "agent-a-mcp", "["+strings.TrimSuffix(strings.Repeat(rpc(1, "ping", "")+",", 101), ",")+"]"),
	mcpCase("mcp posted result", "agent-a-mcp", `{"jsonrpc":"2.0","id":1,"result":{}}`),
	mcpCase("mcp posted error", "agent-a-mcp", `{"jsonrpc":"2.0","error":{"code":-1,"message":"x"}}`),
	mcpCase("mcp tools/list", "agent-a-mcp", rpc(3, "tools/list", "")),
	mcpCase("mcp tools/list without permissions", "agent-b-mcp", rpc(3, "tools/list", "")),
	mcpCase("mcp tools/list bad cursor", "agent-a-mcp", rpc(3, "tools/list", `{"cursor":5}`)),
	mcpCase("mcp tools/call without params", "agent-a-mcp", rpc(4, "tools/call", "")),
	mcpCase("mcp tools/call without a name", "agent-a-mcp", rpc(4, "tools/call", `{"arguments":{}}`)),
	mcpCase("mcp tools/call bad name and arguments", "agent-a-mcp", rpc(4, "tools/call", `{"name":5,"arguments":[1]}`)),
	mcpCase("mcp tools/call bad requestState", "agent-a-mcp", rpc(4, "tools/call", `{"name":"read_instructions","requestState":5}`)),
}

var mcpModernCases = []Scenario{
	one("mcp modern discover", modern(10, "server/discover", "")),
	one("mcp modern discover without Accept or Mcp-Method", func() Step {
		st := modern(10, "server/discover", "")
		st.Header = st.Header[:2]
		return st
	}()),
	one("mcp modern discover without the version header", func() Step {
		st := modern(10, "server/discover", "")
		st.Header = [][2]string{st.Header[0], st.Header[2]}
		return st
	}()),
	one("mcp modern method header mismatch", modern(10, "server/discover", "", [2]string{"Mcp-Method", "tools/list"})),
	one("mcp modern version header mismatch", func() Step {
		st := modern(10, "server/discover", "")
		st.Header[1][1] = "2026-08-01"
		return st
	}()),
	one("mcp modern envelope without capabilities", Step{Method: "POST", Path: mcpPath, Auth: "agent-a-mcp",
		Header: [][2]string{{"Content-Type", "application/json"}, {"MCP-Protocol-Version", "2026-07-28"}, {"Mcp-Method", "tools/list"}},
		Body:   `{"jsonrpc":"2.0","id":11,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`}),
	one("mcp modern envelope bad client name", Step{Method: "POST", Path: mcpPath, Auth: "agent-a-mcp",
		Header: [][2]string{{"Content-Type", "application/json"}, {"MCP-Protocol-Version", "2026-07-28"}, {"Mcp-Method", "tools/list"}},
		Body:   `{"jsonrpc":"2.0","id":11,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":5,"version":"1"}}}}`}),
	one("mcp modern envelope bad log level", Step{Method: "POST", Path: mcpPath, Auth: "agent-a-mcp",
		Header: [][2]string{{"Content-Type", "application/json"}, {"MCP-Protocol-Version", "2026-07-28"}, {"Mcp-Method", "tools/list"}},
		Body:   `{"jsonrpc":"2.0","id":11,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/logLevel":"loud"}}}`}),
	one("mcp modern unsupported version", Step{Method: "POST", Path: mcpPath, Auth: "agent-a-mcp",
		Header: [][2]string{{"Content-Type", "application/json"}, {"MCP-Protocol-Version", "2027-01-01"}, {"Mcp-Method", "tools/list"}},
		Body:   `{"jsonrpc":"2.0","id":11,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2027-01-01","io.modelcontextprotocol/clientCapabilities":{}}}}`}),
	one("mcp modern ping", modern(12, "ping", "")),
	one("mcp modern initialize", modern(12, "initialize", `"protocolVersion":"2026-07-28","capabilities":{},"clientInfo":{"name":"c","version":"1"}`)),
	one("mcp modern tools/list", modern(12, "tools/list", "")),
	one("mcp modern tools/call", modern(13, "tools/call", `"name":"read_note","arguments":{"slug":"instructions/deploy"}`, [2]string{"Mcp-Name", "read_note"})),
	one("mcp modern tools/call base64 name", modern(13, "tools/call", `"name":"read_note","arguments":{"slug":"instructions/deploy"}`, [2]string{"Mcp-Name", "=?base64?cmVhZF9ub3Rl?="})),
	one("mcp modern tools/call without Mcp-Name", modern(13, "tools/call", `"name":"read_note","arguments":{}`)),
	one("mcp modern tools/call Mcp-Name mismatch", modern(13, "tools/call", `"name":"read_note","arguments":{}`, [2]string{"Mcp-Name", "search"})),
	one("mcp modern tools/call bad base64", modern(13, "tools/call", `"name":"read_note","arguments":{}`, [2]string{"Mcp-Name", "=?base64?!!?="})),
	one("mcp modern tools/call error", modern(13, "tools/call", `"name":"read_note","arguments":{}`, [2]string{"Mcp-Name", "read_note"})),
	one("mcp modern notification", Step{Method: "POST", Path: mcpPath, Auth: "agent-a-mcp",
		Header: [][2]string{{"Content-Type", "application/json"}, {"MCP-Protocol-Version", "2026-07-28"}},
		Body:   `{"jsonrpc":"2.0","method":"notifications/cancelled"}`}),
	one("mcp modern subscriptions/listen", modern(15, "subscriptions/listen", `"notifications":{"toolsListChanged":true}`)),
	one("mcp modern subscriptions/listen without a filter", modern(15, "subscriptions/listen", "")),
}

var mcpToolCases = []Scenario{
	toolCase("mcp unknown tool", "agent-a-mcp", "no_such_tool", `{}`),
	toolCase("mcp tool named constructor", "agent-a-mcp", "constructor", `{}`),
	toolCase("mcp tool named __proto__", "agent-a-mcp", "__proto__", `{}`),
	toolCase("mcp permission denied", "agent-b-mcp", "read_note", `{"slug":"x"}`),
	agentBToolCase("mcp read_note without a share", "read_note", `{"slug":"notes/codebase/not-shared","namespace":"private-ns"}`),
	agentBToolCase("mcp save_note through no share", "save_note", `{"title":"t","content":"c","namespace":"private-ns"}`),
	agentBToolCase("mcp list_notes without read access", "list_notes", `{"namespace":"private-ns"}`),
	{Name: "mcp tools/list with some permissions", Steps: []Step{grantAgentB, mcpStep("agent-b-mcp", rpc(3, "tools/list", ""))}},
	toolCase("mcp missing required argument", "agent-a-mcp", "search", `{}`),
	toolCase("mcp argument of the wrong type", "agent-a-mcp", "read_note", `{"slug":"x","limit":"5"}`),
	toolCase("mcp infinite number", "agent-a-mcp", "list_notes", `{"limit":1e400}`),
	toolCase("mcp arguments absent", "agent-a-mcp", "read_instructions", ``),
	mcpCase("mcp arguments null", "agent-a-mcp", rpc(1, "tools/call", `{"name":"read_instructions","arguments":null}`)),
}

var mcpNoteCases = []Scenario{
	toolCase("mcp search", "agent-a-mcp", "search", `{"query":"auth middleware work","namespace":"agent-a"}`),
	agentBToolCase("mcp search everywhere", "search", `{"query":"dream lock","namespace":"*","limit":3}`),
	toolCase("mcp search no results", "agent-a-mcp", "search", `{"query":"zzz","namespace":"quota-ns"}`),
	toolCase("mcp search without access", "agent-a-mcp", "search", `{"query":"x","namespace":"/"}`),
	toolCase("mcp save_note", "agent-a-mcp", "save_note", `{"title":"MCP note","content":"# Hello\nfrom MCP","slug":"notes/mcp-new"}`),
	toolCase("mcp save_note slug from the title", "agent-a-mcp", "save_note", `{"title":"Derived Title","content":"body"}`),
	toolCase("mcp save_note duplicate", "agent-a-mcp", "save_note", `{"title":"Dup","content":"x","slug":"instructions/deploy"}`),
	toolCase("mcp save_note without access", "agent-a-mcp", "save_note", `{"title":"t","content":"c","namespace":"private-ns"}`),
	toolCase("mcp save_note namespace prefix", "agent-a-mcp", "save_note", `{"title":"t","content":"c","slug":"agent-a/x"}`),
	toolCase("mcp list_notes", "agent-a-mcp", "list_notes", `{}`),
	toolCase("mcp list_notes prefix and limit", "agent-a-mcp", "list_notes", `{"prefix":"notes/","limit":1}`),
	toolCase("mcp list_notes empty", "agent-a-mcp", "list_notes", `{"namespace":"empty-ns"}`),
	agentBToolCase("mcp list_notes shared-with-me", "list_notes", `{"namespace":"shared-with-me"}`),
	toolCase("mcp list_notes shared-with-me none", "agent-a-mcp", "list_notes", `{"namespace":"shared-with-me"}`),
	toolCase("mcp list_notes marks shares", "agent-a-mcp", "list_notes", `{"namespace":"private-ns"}`),
	toolCase("mcp read_note", "agent-a-mcp", "read_note", `{"slug":"instructions/deploy"}`),
	toolCase("mcp read_note paginated", "agent-a-mcp", "read_note", `{"slug":"notes/codebase/auth-middleware","offset":2,"limit":1}`),
	toolCase("mcp read_note past the end", "agent-a-mcp", "read_note", `{"slug":"instructions/deploy","offset":500}`),
	toolCase("mcp read_note limit too large", "agent-a-mcp", "read_note", `{"slug":"instructions/deploy","limit":10001}`),
	toolCase("mcp read_note missing", "agent-a-mcp", "read_note", `{"slug":"nope"}`),
	agentBToolCase("mcp read_note through a share", "read_note", `{"slug":"notes/codebase/shared-with-b","namespace":"private-ns"}`),
	toolCase("mcp read_note reserved namespace", "agent-a-mcp", "read_note", `{"slug":"x","namespace":"*"}`),
	toolCase("mcp delete_note", "agent-a-mcp", "delete_note", `{"slug":"instructions/deploy"}`),
	toolCase("mcp delete_note missing", "agent-a-mcp", "delete_note", `{"slug":"nope"}`),
	toolCase("mcp restore_note", "agent-a-mcp", "restore_note", `{"slug":"notes/codebase/deleted-doc"}`),
	toolCase("mcp restore_note tombstone", "agent-a-mcp", "restore_note", `{"slug":"conversations/purged-old"}`),
	toolCase("mcp edit_note", "agent-a-mcp", "edit_note", `{"slug":"instructions/deploy","old_string":"pull main","new_string":"pull origin main"}`),
	toolCase("mcp edit_note not found text", "agent-a-mcp", "edit_note", `{"slug":"instructions/deploy","old_string":"zzz-none","new_string":"x"}`),
	toolCase("mcp edit_note replace_all", "agent-a-mcp", "edit_note", `{"slug":"notes/codebase/auth-middleware","old_string":"e","new_string":"E","replace_all":true}`),
	toolCase("mcp move_note", "agent-a-mcp", "move_note", `{"slug":"instructions/deploy","new_slug":"instructions/deploy-moved"}`),
	toolCase("mcp move_note to another namespace", "agent-a-mcp", "move_note", `{"slug":"instructions/deploy","new_slug":"moved","new_namespace":"shared"}`),
	toolCase("mcp move_note conflict", "agent-a-mcp", "move_note", `{"slug":"instructions/deploy","new_slug":"Notes/Mixed-Case"}`),
	toolCase("mcp move_folder dry run", "agent-a-mcp", "move_folder", `{"from_prefix":"memory-only/","to_prefix":"notes/","dry_run":true}`),
	toolCase("mcp move_folder", "agent-a-mcp", "move_folder", `{"from_prefix":"notes/codebase/","to_prefix":"archive/"}`),
	toolCase("mcp move_folder to the root", "agent-a-mcp", "move_folder", `{"from_prefix":"notes/codebase/","to_prefix":""}`),
	toolCase("mcp move_folder nested", "agent-a-mcp", "move_folder", `{"from_prefix":"notes/","to_prefix":"notes/sub/"}`),
	{
		Name: "mcp move_folder conflicts",
		Steps: []Step{
			mcpStep("agent-a-mcp", call(1, "save_note", `{"title":"c","content":"conflict","slug":"archive/auth-middleware"}`)),
			{Settle: true},
			mcpStep("agent-a-mcp", call(2, "move_folder", `{"from_prefix":"notes/codebase/","to_prefix":"archive/","dry_run":true}`)),
			mcpStep("agent-a-mcp", call(3, "move_folder", `{"from_prefix":"notes/codebase/","to_prefix":"archive/"}`)),
			mcpStep("agent-a-mcp", call(4, "move_folder", `{"from_prefix":"notes/codebase/","to_prefix":"archive/","overwrite_slugs":["archive/other"]}`)),
			mcpStep("agent-a-mcp", call(5, "list_notes", `{"prefix":"archive/"}`)),
		},
	},
	{
		Name: "mcp move_folder overwrite",
		Steps: []Step{
			mcpStep("agent-a-mcp", call(1, "save_note", `{"title":"c","content":"conflict","slug":"archive/auth-middleware"}`)),
			{Settle: true},
			mcpStep("agent-a-mcp", call(2, "move_folder", `{"from_prefix":"notes/codebase/","to_prefix":"archive/","overwrite_slugs":["archive/auth-middleware"]}`)),
		},
	},
	toolCase("mcp grep", "agent-a-mcp", "grep", `{"pattern":"middleware"}`),
	toolCase("mcp grep everywhere with context", "agent-a-mcp", "grep", `{"pattern":"sessions","namespace":"*","context":1}`),
	toolCase("mcp grep regex blocks", "agent-a-mcp", "grep", `{"pattern":"^(## auth|sessions refresh)","regex":true,"context":0}`),
	toolCase("mcp grep no match", "agent-a-mcp", "grep", `{"pattern":"zzz-none"}`),
	toolCase("mcp grep bad regex", "agent-a-mcp", "grep", `{"pattern":"(a","regex":true}`),
	toolCase("mcp read_instructions", "agent-a-mcp", "read_instructions", `{}`),
	agentBToolCase("mcp read_instructions with dreams and a soul", "read_instructions", `{}`),
	toolCase("mcp save_instructions", "agent-a-mcp", "save_instructions", `{"content":"New instructions ​."}`),
}

// mcpClientScript is a contract/mcpclient script.
func mcpClientScript(mode, calls string) string {
	return fmt.Sprintf(`{"bearer":"{{agent-a-mcp}}","mode":%q,"calls":%s}`, mode, calls)
}

const mcpClientCalls = `[{"op":"listTools"},{"op":"callTool","name":"read_note","arguments":{"slug":"instructions/deploy"}},` +
	`{"op":"callTool","name":"read_note","arguments":{}},{"op":"callTool","name":"no_such_tool"},{"op":"ping"}]`

var mcpClientCases = []Scenario{
	one("mcp SDK client, 2025 era", Step{Label: "mcp client legacy", MCPClient: mcpClientScript("legacy", mcpClientCalls)}),
	one("mcp SDK client, 2026-07-28 era", Step{Label: "mcp client auto", MCPClient: mcpClientScript("auto", mcpClientCalls)}),
	one("mcp SDK client without a bearer", Step{Label: "mcp client no bearer", MCPClient: `{"mode":"auto","calls":[]}`}),
}
