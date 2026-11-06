package mcp

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/events"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// The protocol cases below are byte-exact answers of the Node server (the
// TypeScript SDK) for the same requests; the contract harness checks the
// rest against Node directly.

func testServer(t *testing.T) (*Server, pgxmock.PgxPoolIface, *bytes.Buffer) {
	mock := testutil.NewMock(t)
	var out bytes.Buffer
	s := &Server{DB: mock, Log: logger.New(mock, actors.NewCache(mock), ""), Events: events.Nop{}, Stdout: &out}
	return s, mock, &out
}

// do sends one request through the JSON body parser, as the server chain
// does, with agent-a authenticated holding perms.
func do(s *Server, method, body string, header map[string]string, perms ...string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/mcp", nil)
	} else {
		r = httptest.NewRequest(method, "/mcp", strings.NewReader(body))
	}
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := httpapi.StateOf(r)
		st.MCPAgent, st.MCPActorID, st.MCPPermissions = "agent-a", 0, perms
		s.ServeHTTP(w, r)
	})
	httpapi.WithState(httpapi.ParseJSONBody(inner)).ServeHTTP(w, r)
	return w
}

func anyArgs(n int) []any {
	args := make([]any, n)
	for i := range args {
		args[i] = pgxmock.AnyArg()
	}
	return args
}

var legacy = map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream"}

func TestLegacyRequestsAreSSE(t *testing.T) {
	s, mock, _ := testServer(t)
	cases := []struct{ body, want string }{
		{`{"jsonrpc":"2.0","id":2,"method":"ping","params":{"x":1}}`, `{"result":{},"jsonrpc":"2.0","id":2}`},
		{`{"jsonrpc":"2.0","id":"q","method":"foo/bar"}`, `{"jsonrpc":"2.0","id":"q","error":{"code":-32601,"message":"Method not found"}}`},
		{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`,
			`{"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"llm-memory","version":"2.0.0"},"instructions":"` + instructions + `"},"jsonrpc":"2.0","id":1}`},
		{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`,
			`{"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"llm-memory","version":"2.0.0"},"instructions":"` + instructions + `"},"jsonrpc":"2.0","id":1}`},
		{`{"jsonrpc":"2.0","id":1,"method":"initialize"}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"[\n  {\n    \"expected\": \"object\",\n    \"code\": \"invalid_type\",\n    \"path\": [\n      \"params\"\n    ],\n    \"message\": \"Invalid input: expected object, received undefined\"\n  }\n]"}}`},
	}
	for _, c := range cases {
		mock.ExpectExec("UPDATE actors SET active_since").WithArgs(pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("UPDATE", 1)).Maybe()
		w := do(s, "POST", c.body, legacy)
		if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" || w.Header().Get("Cache-Control") != "no-cache, no-transform" {
			t.Fatalf("%s: %d %v", c.body, w.Code, w.Header())
		}
		if got, want := w.Body.String(), "event: message\ndata: "+c.want+"\n\n"; got != want {
			t.Errorf("%s:\n got %q\nwant %q", c.body, got, want)
		}
	}
}

func TestEntryRefusals(t *testing.T) {
	s, mock, _ := testServer(t)
	cases := []struct {
		method, body string
		header       map[string]string
		status       int
		want         string
		logged       bool
	}{
		{"GET", "", nil, 405, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Method not allowed."},"id":null}`, false},
		{"POST", `{}`, map[string]string{"Content-Type": "text/plain"}, 415, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Unsupported Media Type: Content-Type must be application/json"},"id":null}`, true},
		{"POST", `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Content-Type": "application/json"}, 406, `{"jsonrpc":"2.0","error":{"code":-32000,"message":"Not Acceptable: Client must accept both application/json and text/event-stream"},"id":null}`, false},
		{"POST", `[]`, legacy, 400, `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Bad Request: empty JSON-RPC batch"},"id":null}`, true},
		{"POST", `{"jsonrpc":"2.0","id":9,"method":"ping","x":1}`, legacy, 400, `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Bad Request: the request body is not a valid JSON-RPC message"},"id":9}`, true},
		{"POST", `{"jsonrpc":"2.0","id":12345678901234567890,"method":"ping"}`, legacy, 400, `{"jsonrpc":"2.0","error":{"code":-32600,"message":"Bad Request: the request body is not a valid JSON-RPC message"},"id":12345678901234567000}`, true},
		{"POST", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, legacy, 202, ``, false},
		{"POST", `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`, map[string]string{"Content-Type": "application/json", "Accept": "application/json, text/event-stream", "MCP-Protocol-Version": "2024-01-01"}, 400,
			`{"jsonrpc":"2.0","error":{"code":-32000,"message":"Bad Request: Unsupported protocol version: 2024-01-01 (supported versions: 2025-11-25, 2025-06-18, 2025-03-26, 2024-11-05, 2024-10-07)"},"id":null}`, false},
	}
	for _, c := range cases {
		if c.logged {
			mock.ExpectExec("INSERT INTO error_log").WithArgs(anyArgs(8)...).WillReturnResult(pgxmock.NewResult("INSERT", 1))
		}
		w := do(s, c.method, c.body, c.header)
		if w.Code != c.status || w.Body.String() != c.want {
			t.Errorf("%s %s: %d %q", c.method, c.body, w.Code, w.Body.String())
		}
		if c.status == 405 && w.Header().Get("Allow") != "POST" {
			t.Errorf("405 without Allow")
		}
	}
	testutil.WaitMet(t, mock)
}

func TestModernDiscoverAndToolsList(t *testing.T) {
	s, mock, out := testServer(t)
	mock.ExpectExec("UPDATE actors SET active_since").WithArgs(pgxmock.AnyArg()).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	h := map[string]string{"Content-Type": "application/json", "MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "server/discover"}
	w := do(s, "POST", `{"jsonrpc":"2.0","id":10,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"modern-test","version":"2.0"}}}}`, h)
	want := `{"result":{"supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"instructions":"` + instructions + `","resultType":"complete","ttlMs":0,"cacheScope":"private","_meta":{"io.modelcontextprotocol/serverInfo":{"name":"llm-memory","version":"2.0.0"}}},"jsonrpc":"2.0","id":10}`
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" || w.Body.String() != want {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	if !strings.HasPrefix(out.String(), `[mcp] connect {"agent":"agent-a","ip":"192.0.2.1","era":"modern","method":"server/discover","protocolVersion":"2026-07-28","clientName":"modern-test","clientVersion":"2.0","userAgent":null}`) {
		t.Errorf("connect line %q", out.String())
	}

	h["Mcp-Method"] = "tools/list"
	w = do(s, "POST", `{"jsonrpc":"2.0","id":12,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, h, "mcp_agent_status")
	v, err := jsutil.Parse(w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	result := v.(*jsutil.Object).Value("result").(*jsutil.Object)
	if got := strings.Join(result.Keys(), ","); got != "tools,resultType,ttlMs,cacheScope,_meta" {
		t.Errorf("result keys %s", got)
	}
	var names []string
	for _, tool := range result.Value("tools").([]any) {
		names = append(names, tool.(*jsutil.Object).Value("name").(string))
	}
	if strings.Join(names, ",") != "agent_status,update_expertise,update_profile,activity_start,activity_stop" {
		t.Errorf("tools %v", names)
	}

	// A modern ping is not in the 2026 registry: 404.
	h["Mcp-Method"] = "ping"
	w = do(s, "POST", `{"jsonrpc":"2.0","id":13,"method":"ping","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`, h)
	if w.Code != 404 || w.Body.String() != `{"jsonrpc":"2.0","id":13,"error":{"code":-32601,"message":"Method not found"}}` {
		t.Errorf("modern ping %d %q", w.Code, w.Body.String())
	}
	testutil.WaitMet(t, mock)
}

func TestClassifyRejections(t *testing.T) {
	cases := []struct {
		body string
		h    headers
		cell string
	}{
		{`[{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":1}}}]`, headers{}, "batch-with-modern-element"},
		{`[{"x":1}]`, headers{}, "batch-with-invalid-element"},
		{`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, headers{version: "2026-07-28", hasVersion: true}, "initialize-with-modern-header"},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, headers{version: "foo", hasVersion: true}, "modern-header-without-claim"},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`, headers{}, "envelope-invalid"},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			headers{version: "2026-07-29", hasVersion: true}, "header-body-version-mismatch"},
		{`{"jsonrpc":"2.0","method":"n","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":5}}}`, headers{}, "notification-envelope-invalid"},
	}
	for _, c := range cases {
		v, err := jsutil.Parse([]byte(c.body))
		if err != nil {
			t.Fatal(err)
		}
		out := classify(v, c.h)
		if out.kind != outcomeReject || out.cell != c.cell {
			t.Errorf("%s: %+v, want %s", c.body, out, c.cell)
		}
	}
}

func TestIsJSONContentType(t *testing.T) {
	for ct, want := range map[string]bool{
		"application/json":                true,
		"APPLICATION/JSON":                true,
		"application/json; charset=utf-8": true,
		"application/json;":               true,
		"application/json; a=b, c":        false,
		"application/vnd.api+json":        false,
		"text/plain":                      false,
		"":                                false,
	} {
		if got := isJSONContentType(ct); got != want {
			t.Errorf("%q: %v", ct, got)
		}
	}
}

func TestDecodeNameHeader(t *testing.T) {
	for in, want := range map[string]string{"echo": "echo", "=?base64?ZWNobw==?=": "echo", "=?base64??=": "", "=?base64?=": ""} {
		got, ok := decodeNameHeader(in)
		if !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"=?base64?!!?=", "=?base64?ZWNobw?=", "=?base64?/w==?="} {
		if _, ok := decodeNameHeader(in); ok {
			t.Errorf("%q decoded", in)
		}
	}
}

// A client that goes away mid-batch stops the writes, not the tool: the call
// runs to its end on a live context, and nothing is written after the
// handler returned.
func TestLegacyBatchToolOutlivesTheClient(t *testing.T) {
	s, _, _ := testServer(t)
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	saved := toolHandlers["read_instructions"]
	toolHandlers["read_instructions"] = func(_ *Server, ctx context.Context, _ *jsutil.Object, _ caller) (string, error) {
		close(started)
		<-release
		finished <- ctx.Err()
		return "done", nil
	}
	t.Cleanup(func() { toolHandlers["read_instructions"] = saved })
	workersDone := make(chan struct{})
	s.callsDone = func() { close(workersDone) }

	ctx, disconnect := context.WithCancel(context.Background())
	body := `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_instructions"}}]`
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body)).WithContext(ctx)
	for k, v := range legacy {
		r.Header.Set(k, v)
	}
	w := &countingWriter{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			st := httpapi.StateOf(r)
			st.MCPAgent, st.MCPPermissions = "agent-a", []string{"mcp_read_note"}
			s.ServeHTTP(w, r)
		})
		httpapi.WithState(httpapi.ParseJSONBody(inner)).ServeHTTP(w, r)
	}()

	<-started
	disconnect()
	<-done
	w.closed.Store(true)
	close(release)
	if err := <-finished; err != nil {
		t.Fatalf("the tool's context was cancelled: %v", err)
	}
	// Every worker has exited before the writes are counted.
	<-workersDone
	if w.late.Load() != 0 {
		t.Fatalf("%d writes after the handler returned", w.late.Load())
	}
	if got := w.Body.String(); got != "event: message\ndata: {\"result\":{},\"jsonrpc\":\"2.0\",\"id\":1}\n\n" {
		t.Fatalf("body %q", got)
	}
}

type countingWriter struct {
	*httptest.ResponseRecorder
	closed atomic.Bool
	late   atomic.Int32
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if c.closed.Load() {
		c.late.Add(1)
	}
	return c.ResponseRecorder.Write(b)
}

func (c *countingWriter) Flush() {}
