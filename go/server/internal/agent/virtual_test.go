package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/virtualagent"
)

// fakeAgents records the calls the routes make to the virtual-agent
// service.
type fakeAgents struct {
	limits    map[string]virtualagent.RateLimit
	limitErr  error
	looked    []string
	invoked   []string
	opts      []virtualagent.InvokeOptions
	ctxErr    error
	result    *virtualagent.InvokeResult
	invokeErr error
}

func (f *fakeAgents) EffectiveRateLimit(_ context.Context, agent string) (virtualagent.RateLimit, error) {
	f.looked = append(f.looked, agent)
	if f.limitErr != nil {
		return virtualagent.RateLimit{}, f.limitErr
	}
	if rl, ok := f.limits[agent]; ok {
		return rl, nil
	}
	return virtualagent.RateLimit{Limit: 10, WindowMs: 60000, CooldownMs: 30000}, nil
}

func (f *fakeAgents) Invoke(ctx context.Context, agent string, opts virtualagent.InvokeOptions) (*virtualagent.InvokeResult, error) {
	f.invoked = append(f.invoked, agent)
	f.opts = append(f.opts, opts)
	f.ctxErr = ctx.Err()
	if f.invokeErr != nil {
		return nil, f.invokeErr
	}
	return f.result, nil
}

func newVirtualRoutes(t *testing.T) (*Routes, *fakeAgents) {
	rt, mock, _ := newRoutes(t, nil)
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
	fa := &fakeAgents{}
	rt.Agents = fa
	return rt, fa
}

func TestVirtualDirectErrors(t *testing.T) {
	long := ""
	for i := 0; i < 201; i++ {
		long += "a"
	}
	// 200 UTF-16 units: 100 characters outside the BMP.
	astral := ""
	for i := 0; i < 100; i++ {
		astral += "😀"
	}
	many := "["
	for i := 0; i < 201; i++ {
		if i > 0 {
			many += ","
		}
		many += `"a"`
	}
	many += "]"

	rt, fa := newVirtualRoutes(t)
	cases := []struct {
		name      string
		fn        func(http.ResponseWriter, *http.Request) error
		body      string
		agent     string
		status    int
		code, msg string
	}{
		{"rate admin", rt.rateLimit, ``, "", 401, "UNAUTHORIZED", "Agent session required"},
		{"rate missing", rt.rateLimit, `{}`, "a", 400, "BAD_REQUEST", rateLimitSlugsError},
		{"rate empty", rt.rateLimit, `{"agents":[]}`, "a", 400, "BAD_REQUEST", rateLimitSlugsError},
		{"rate string", rt.rateLimit, `{"agents":"x"}`, "a", 400, "BAD_REQUEST", rateLimitSlugsError},
		{"rate array body", rt.rateLimit, `[["x"]]`, "a", 400, "BAD_REQUEST", rateLimitSlugsError},
		{"rate too many", rt.rateLimit, `{"agents":` + many + `}`, "a", 400, "BAD_REQUEST", rateLimitBatchError},
		{"rate number entry", rt.rateLimit, `{"agents":["x",5]}`, "a", 400, "BAD_REQUEST", rateLimitSlugError},
		{"rate empty entry", rt.rateLimit, `{"agents":["x",""]}`, "a", 400, "BAD_REQUEST", rateLimitSlugError},
		{"rate long entry", rt.rateLimit, `{"agents":["` + long + `"]}`, "a", 400, "BAD_REQUEST", rateLimitSlugError},
		{"rate astral entry", rt.rateLimit, `{"agents":["` + astral + `x"]}`, "a", 400, "BAD_REQUEST", rateLimitSlugError},
		{"tick admin", rt.tick, ``, "", 401, "UNAUTHORIZED", "Agent session required"},
		{"tick neither", rt.tick, `{}`, "a", 400, "BAD_REQUEST", tickNeedsInputError},
		{"tick empty perception", rt.tick, `{"perception":"","messages":[]}`, "a", 400, "BAD_REQUEST", tickNeedsInputError},
		{"tick perception not string", rt.tick, `{"perception":["x"]}`, "a", 400, "BAD_REQUEST", tickNeedsInputError},
		{"tick both", rt.tick, `{"perception":"x","messages":[{}]}`, "a", 400, "BAD_REQUEST", tickBothInputsError},
		{"tick tools null", rt.tick, `{"perception":"x","tools":null}`, "a", 400, "BAD_REQUEST", tickToolsError},
		{"tick tools object", rt.tick, `{"perception":"x","tools":{}}`, "a", 400, "BAD_REQUEST", tickToolsError},
		{"tick system null", rt.tick, `{"perception":"x","system":null}`, "a", 400, "BAD_REQUEST", tickSystemError},
		{"tick system number", rt.tick, `{"messages":[1],"system":5}`, "a", 400, "BAD_REQUEST", tickSystemError},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		if err := c.fn(rec, request(c.body, c.agent, 7)); err != nil {
			t.Errorf("%s: err %v", c.name, err)
			continue
		}
		if rec.Code != c.status || rec.Body.String() != errorBody(c.code, c.msg) {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	if len(fa.looked) != 0 || len(fa.invoked) != 0 {
		t.Errorf("service called: %v %v", fa.looked, fa.invoked)
	}

	// A 200-unit astral slug is within the bound.
	rec := httptest.NewRecorder()
	if err := rt.rateLimit(rec, request(`{"agents":["`+astral+`"]}`, "a", 7)); err != nil || rec.Code != 200 {
		t.Errorf("astral 200: %v %d %s", err, rec.Code, rec.Body)
	}
}

func TestVirtualNoBody(t *testing.T) {
	rt, _ := newVirtualRoutes(t)
	for name, c := range map[string]struct {
		fn   func(http.ResponseWriter, *http.Request) error
		want string
	}{
		"rate-limit": {rt.rateLimit, "Cannot destructure property 'agents' of 'req.body' as it is undefined."},
		"tick":       {rt.tick, "Cannot destructure property 'perception' of 'req.body' as it is undefined."},
	} {
		err := c.fn(httptest.NewRecorder(), request(``, "a", 7))
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Slugs are looked up once each in first-seen order; the answer is in
// JavaScript key order (array-index keys first), with __proto__ left out
// and NaN written as null.
func TestRateLimitAnswer(t *testing.T) {
	rt, fa := newVirtualRoutes(t)
	fa.limits = map[string]virtualagent.RateLimit{
		"bad": {Limit: math.NaN(), WindowMs: math.NaN(), CooldownMs: 5000},
	}
	rec := httptest.NewRecorder()
	body := `{"agents":["zeta","7","bad","zeta","__proto__","alpha","7"]}`
	if err := rt.rateLimit(rec, request(body, "a", 7)); err != nil {
		t.Fatal(err)
	}
	want := `{"limits":{"7":{"limit":10,"window_ms":60000,"cooldown_ms":30000},` +
		`"zeta":{"limit":10,"window_ms":60000,"cooldown_ms":30000},` +
		`"bad":{"limit":null,"window_ms":null,"cooldown_ms":5000},` +
		`"alpha":{"limit":10,"window_ms":60000,"cooldown_ms":30000}}}`
	if rec.Code != 200 || rec.Body.String() != want {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
	looked := []string{"zeta", "7", "bad", "__proto__", "alpha"}
	if len(fa.looked) != len(looked) {
		t.Fatalf("looked %v", fa.looked)
	}
	for i := range looked {
		if fa.looked[i] != looked[i] {
			t.Errorf("looked %v", fa.looked)
		}
	}
}

// A config error (a missing key) is the route's error, logged as a 500.
func TestRateLimitConfigError(t *testing.T) {
	rt, fa := newVirtualRoutes(t)
	fa.limitErr = errors.New("Config key not found: virtual_agent_rate_limit")
	err := rt.rateLimit(httptest.NewRecorder(), request(`{"agents":["x"]}`, "a", 7))
	if err != fa.limitErr {
		t.Errorf("%v", err)
	}
}

func TestTickOptions(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		check func(virtualagent.InvokeOptions) bool
	}{
		{"perception", `{"perception":"hi","messages":[]}`, func(o virtualagent.InvokeOptions) bool {
			return o.UserMessage == "hi" && o.Messages == nil && o.Tools == nil && o.SystemPrompt == nil &&
				o.Context == "tick" && !o.Retry && !o.SkipRateLimit && !o.SkipCostLimit
		}},
		{"messages tools system", `{"messages":[{"role":"user","content":"x"}],"tools":[{"name":"t"}],"system":"sys","perception":5}`,
			func(o virtualagent.InvokeOptions) bool {
				return o.UserMessage == "" && jsutil.Marshal(o.Messages) == `[{"role":"user","content":"x"}]` &&
					jsutil.Marshal(o.Tools) == `[{"name":"t"}]` && o.SystemPrompt != nil &&
					*o.SystemPrompt == providers.TextPrompt("sys")
			}},
		{"empty tools and system", `{"perception":"p","tools":[],"system":""}`, func(o virtualagent.InvokeOptions) bool {
			return o.Tools == nil && o.SystemPrompt != nil && *o.SystemPrompt == providers.TextPrompt("")
		}},
	}
	for _, c := range cases {
		rt, fa := newVirtualRoutes(t)
		fa.result = &virtualagent.InvokeResult{Text: "ok", Usage: providers.Usage{InputTokens: 1}}
		if err := rt.tick(httptest.NewRecorder(), request(c.body, "npc", 7)); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(fa.invoked) != 1 || fa.invoked[0] != "npc" || !c.check(fa.opts[0]) {
			t.Errorf("%s: %v %+v", c.name, fa.invoked, fa.opts)
		}
	}
}

// The answer is Node's {agent, text, tool_calls, usage, cost}, without
// finish_reason and truncated.
func TestTickAnswer(t *testing.T) {
	rt, fa := newVirtualRoutes(t)
	fa.result = &virtualagent.InvokeResult{
		Text:         nil,
		ToolCalls:    []providers.ToolCall{{ID: "c1", Name: "speak", Input: jsutil.NewObject()}},
		Usage:        providers.Usage{InputTokens: 12, OutputTokens: 3},
		Cost:         0.000125,
		FinishReason: "tool_use",
		Truncated:    true,
	}
	rec := httptest.NewRecorder()
	if err := rt.tick(rec, request(`{"perception":"p"}`, "npc", 7)); err != nil {
		t.Fatal(err)
	}
	want := `{"agent":"npc","text":null,"tool_calls":[{"id":"c1","name":"speak","input":{}}],` +
		`"usage":{"input_tokens":12,"output_tokens":3},"cost":0.000125}`
	if rec.Code != 200 || rec.Body.String() != want {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

// The refusals are matched on their text, as Node's message.includes did,
// so another error with those words maps the same way. Anything else is the
// route's error.
func TestTickErrors(t *testing.T) {
	rateErr := &virtualagent.RateLimitedError{Agent: "npc"}
	costErr := &virtualagent.OverCostLimitError{Agent: "npc", Reason: "daily limit $1.00 reached ($1.20 spent)"}
	cases := []struct {
		name      string
		err       error
		status    int
		code, msg string
	}{
		{"rate", rateErr, 429, "RATE_LIMITED", "Agent npc is rate-limited"},
		{"cost", costErr, 402, "COST_LIMIT", costErr.Error()},
		{"cost for a rate-limited name", &virtualagent.OverCostLimitError{Agent: "rate-limited-npc", Reason: "x"},
			429, "RATE_LIMITED", "Agent rate-limited-npc over cost limit: x"},
		{"provider text", errors.New("upstream says: over cost limit"), 402, "COST_LIMIT", "upstream says: over cost limit"},
	}
	for _, c := range cases {
		rt, fa := newVirtualRoutes(t)
		fa.invokeErr = c.err
		rec := httptest.NewRecorder()
		if err := rt.tick(rec, request(`{"perception":"p"}`, "npc", 7)); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if rec.Code != c.status || rec.Body.String() != errorBody(c.code, c.msg) {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}

	rt, fa := newVirtualRoutes(t)
	fa.invokeErr = errors.New("Agent not found: npc")
	if err := rt.tick(httptest.NewRecorder(), request(`{"perception":"p"}`, "npc", 7)); err != fa.invokeErr {
		t.Errorf("other: %v", err)
	}
}

// A client that hangs up does not cancel the call, as Node's ran on.
func TestTickSurvivesHangUp(t *testing.T) {
	rt, fa := newVirtualRoutes(t)
	fa.result = &virtualagent.InvokeResult{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("POST", "/v1/agent/tick", nil).WithContext(ctx)
	r = httpapi.WithStateValue(r, &httpapi.State{AuthenticatedAgent: "npc", ActorID: 7, Body: json.RawMessage(`{"perception":"p"}`)})
	if err := rt.tick(httptest.NewRecorder(), r); err != nil {
		t.Fatal(err)
	}
	if len(fa.invoked) != 1 || fa.ctxErr != nil {
		t.Errorf("Invoke %v saw %v", fa.invoked, fa.ctxErr)
	}
}
