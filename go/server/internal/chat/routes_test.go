package chat

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
)

func request(body, agent string) *http.Request {
	r := httptest.NewRequest("POST", "/v1/chat/x", nil)
	st := &httpapi.State{AuthenticatedAgent: agent}
	if body != "" {
		st.Body = json.RawMessage(body)
	}
	return httpapi.WithStateValue(r, st)
}

// Every send check Node answered with res.status(...).json() is written
// directly, before the service runs, in Node's order.
func TestSendValidation(t *testing.T) {
	rt := &Routes{}
	long := strings.Repeat("x", 65537)
	cases := []struct {
		body, agent string
		status      int
		code, msg   string
	}{
		{`{"to_agents":["b"],"message":"m","from_agent":"other"}`, "agent-a", 403, "IDENTITY_MISMATCH", "from_agent does not match authenticated agent"},
		{`{"stable_context":"x"}`, "agent-a", 403, "FORBIDDEN", "stable_context is only supported for sim engine chat"},
		{`{"stable_context":5}`, "salem-engine", 400, "BAD_REQUEST", "stable_context must be a string"},
		{`{"stable_context":"` + long + `"}`, "salem-engine", 400, "BAD_REQUEST", "stable_context exceeds 65536 chars"},
		{`{"stable_context":null,"ephemeral_context":"x"}`, "agent-a", 403, "FORBIDDEN", "ephemeral_context is only supported for sim engine chat"},
		{`{"ephemeral_context":["x"]}`, "salem-engine", 400, "BAD_REQUEST", "ephemeral_context must be a string"},
		{`{"ephemeral_context":"` + long + `"}`, "salem-engine", 400, "BAD_REQUEST", "ephemeral_context exceeds 65536 chars"},
		{`{"tool_calls":{}}`, "agent-a", 400, "BAD_REQUEST", "tool_calls must be an array"},
		{`{"tool_calls":"[]"}`, "agent-a", 400, "BAD_REQUEST", "tool_calls must be an array"},
		{`{"tools_offered":5}`, "agent-a", 400, "BAD_REQUEST", "tools_offered must be an array"},
		{`{"tool_call_id":5}`, "agent-a", 400, "BAD_REQUEST", "tool_call_id must be a string"},
		{`{"tool_call_results":{}}`, "agent-a", 400, "BAD_REQUEST", "tool_call_results must be an array"},
		{`{"tool_call_results":[]}`, "agent-a", 400, "BAD_REQUEST", "tool_call_results must be non-empty when present"},
		{`{"tool_call_results":[null]}`, "agent-a", 400, "BAD_REQUEST", "tool_call_results entries must be { id: non-empty string, content: string }"},
		{`{"tool_call_results":[["c1","x"]]}`, "agent-a", 400, "BAD_REQUEST", "tool_call_results entries must be { id: non-empty string, content: string }"},
		{`{"tool_call_results":[{"id":"","content":"x"}]}`, "agent-a", 400, "BAD_REQUEST", "tool_call_results entries must be { id: non-empty string, content: string }"},
		{`{"tool_call_results":[{"id":"c1"}]}`, "agent-a", 400, "BAD_REQUEST", "tool_call_results entries must be { id: non-empty string, content: string }"},
		{`{"tool_call_results":[{"id":"c1","content":"x"}],"tool_call_id":"c2"}`, "agent-a", 400, "BAD_REQUEST", "tool_call_results and tool_call_id are mutually exclusive"},
		{`{"tool_call_results":[{"id":"c1","content":"x"}],"message":"m"}`, "agent-a", 400, "BAD_REQUEST", "tool_call_results is mutually exclusive with message"},
		{`{"persist_only":true}`, "agent-a", 400, "BAD_REQUEST", "persist_only requires tool_call_results"},
		{`{"persist_only":true,"tool_call_results":null}`, "agent-a", 400, "BAD_REQUEST", "persist_only requires tool_call_results"},
		{`{"scene_id":"not-a-uuid"}`, "agent-a", 400, "BAD_REQUEST", "scene_id must be a UUID string"},
		{`{"scene_id":7}`, "agent-a", 400, "BAD_REQUEST", "scene_id must be a UUID string"},
		{`{"scene_structure":"` + strings.Repeat("s", 101) + `"}`, "agent-a", 400, "BAD_REQUEST", "scene_structure must be a string up to 100 chars"},
		{`{"conversation_id":" ` + strings.Repeat("c", 64) + `"}`, "agent-a", 400, "BAD_REQUEST", "conversation_id must be a string up to 64 chars"},
		{`{"sim_actor_id":5}`, "agent-a", 400, "BAD_REQUEST", "sim_actor_id must be a string up to 100 chars"},
		{`{"sim_actor_id":"` + strings.Repeat("i", 101) + `"}`, "agent-a", 400, "BAD_REQUEST", "sim_actor_id must be a string up to 100 chars"},
		{`{"sim_actor_name":false}`, "agent-a", 400, "BAD_REQUEST", "sim_actor_name must be a string up to 100 chars"},
		{`{"history_scope":"agent"}`, "agent-a", 400, "BAD_REQUEST", `history_scope must be "scene" when present`},
		{`{"history_scope":""}`, "agent-a", 400, "BAD_REQUEST", `history_scope must be "scene" when present`},
		{`{"tool_call_results":[{"id":"c1","content":"x"}],"persist_only":true,"wait":true}`, "agent-a", 400, "BAD_REQUEST", "persist_only is incompatible with wait=true (nothing to wait for)"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		if err := rt.send(rec, request(c.body, c.agent)); err != nil {
			t.Errorf("%.80s: err %v", c.body, err)
			continue
		}
		want := `{"error":{"code":"` + c.code + `","message":` + string(mustJSON(c.msg)) + `}}`
		if rec.Code != c.status || rec.Body.String() != want {
			t.Errorf("%.80s: %d %s", c.body, rec.Code, rec.Body)
		}
	}
}

func mustJSON(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

// The port refuses a field of a JSON type Node passed on (go-server note,
// Known differences); a blank padding past 100 units fits once trimmed.
func TestSendFieldTypes(t *testing.T) {
	rt := &Routes{}
	cases := map[string]string{
		`{"message":5}`:                   "message must be a string",
		`{"message":false}`:               "message must be a string",
		`{"message":"m","to_agents":[5]}`: "to_agents must be an array of strings",
		`{"message":"m","channel":["x"]}`: "channel must be a string",
		`{"from_agent":5}`:                "from_agent must be a string",
	}
	for body, want := range cases {
		err := rt.send(httptest.NewRecorder(), request(body, ""))
		if status(err) != 400 || err.Error() != want {
			t.Errorf("%s: %v", body, err)
		}
	}
}

func TestSendFieldValues(t *testing.T) {
	f := httpapi.Fields{}
	json.Unmarshal([]byte(`{"to_agents":[" Agent-B ","*","x!"]}`), &f)
	got, err := toAgentsField(f)
	if err != nil || !got.Given || !got.IsArray || strings.Join(got.Names, ",") != "agent-b,*,x" {
		t.Errorf("array: %+v %v", got, err)
	}
	for body, want := range map[string]ToAgents{
		`{"to_agents":"*"}`:   {Given: true, Names: []string{"*"}},
		`{"to_agents":"bob"}`: {Given: true},
		`{"to_agents":5}`:     {Given: true},
		`{"to_agents":""}`:    {},
		`{"to_agents":0}`:     {},
		`{}`:                  {},
	} {
		f := httpapi.Fields{}
		json.Unmarshal([]byte(body), &f)
		got, err := toAgentsField(f)
		if err != nil || got.Given != want.Given || got.IsArray != want.IsArray || strings.Join(got.Names, ",") != strings.Join(want.Names, ",") {
			t.Errorf("%s: %+v %v", body, got, err)
		}
	}

	pad := " " + strings.Repeat("i", 100) + " "
	f = httpapi.Fields{}
	json.Unmarshal([]byte(`{"sim_actor_id":"`+pad+`","sim_actor_name":"  ","conversation_id":" sc-1 "}`), &f)
	if v, msg := trimmedOptional(f, "sim_actor_id", 100, false, "bad"); msg != "" || v == nil || len(*v) != 100 {
		t.Errorf("sim_actor_id: %v %q", v, msg)
	}
	if v, msg := trimmedOptional(f, "sim_actor_name", 100, false, "bad"); msg != "" || v != nil {
		t.Errorf("sim_actor_name: %v %q", v, msg)
	}
	if v, msg := trimmedOptional(f, "conversation_id", 64, true, "bad"); msg != "" || v == nil || *v != "sc-1" {
		t.Errorf("conversation_id: %v %q", v, msg)
	}
}

func TestAfterIDField(t *testing.T) {
	for raw, ok := range map[string]bool{`0`: true, `5`: true, `-0`: true, `1e2`: true, `2.0`: true, `-1`: false, `1.5`: false, `"5"`: false,
		`null`: false, `true`: false, `[5]`: false, `1e400`: false} {
		if _, got := afterIDField(json.RawMessage(raw)); got != ok {
			t.Errorf("%s: %v", raw, got)
		}
	}
	if v, ok := afterIDField(nil); v != nil || !ok {
		t.Error("absent")
	}
}

func TestNoBodyErrorsAreV8s(t *testing.T) {
	rt := &Routes{}
	if err := rt.send(httptest.NewRecorder(), request("", "agent-a")); err == nil || err.Error() != "Cannot read properties of undefined (reading 'message')" {
		t.Errorf("send: %v", err)
	}
	for _, fn := range []func(http.ResponseWriter, *http.Request) error{rt.receive, rt.ack, rt.status} {
		if err := fn(httptest.NewRecorder(), request("", "agent-a")); err == nil || err.Error() != "Cannot read properties of undefined (reading 'agent')" {
			t.Errorf("got %v", err)
		}
	}
}

func TestReceiveRejectsAfterID(t *testing.T) {
	rt := &Routes{}
	rec := httptest.NewRecorder()
	if err := rt.receive(rec, request(`{"after_id":null}`, "agent-a")); err != nil || rec.Code != 400 ||
		rec.Body.String() != `{"error":{"code":"BAD_REQUEST","message":"after_id must be a non-negative integer"}}` {
		t.Errorf("%v %d %s", err, rec.Code, rec.Body)
	}
}

func TestReplyErrors(t *testing.T) {
	resumes := 30.0
	cases := []struct {
		err    error
		status int
		body   string
	}{
		{&RateLimitedError{Message: "slow down", ResumesInSeconds: &resumes}, 429, `{"error":{"code":"RATE_LIMITED","message":"slow down","resumes_in_seconds":30}}`},
		{&RateLimitedError{}, 429, `{"error":{"code":"RATE_LIMITED","message":"Rate limited"}}`},
		{&OverBudgetError{Message: "cap"}, 402, `{"error":{"code":"OVER_BUDGET","message":"cap"}}`},
		{&OverBudgetError{}, 402, `{"error":{"code":"OVER_BUDGET","message":"Cost budget exceeded"}}`},
		{errors.New("provider down"), 502, `{"error":{"code":"REPLY_FAILED","message":"provider down"}}`},
		{errors.New(""), 502, `{"error":{"code":"REPLY_FAILED","message":"virtual agent reply failed"}}`},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		writeReplyError(rec, c.err)
		if rec.Code != c.status || rec.Body.String() != c.body {
			t.Errorf("%v: %d %s", c.err, rec.Code, rec.Body)
		}
	}
}
