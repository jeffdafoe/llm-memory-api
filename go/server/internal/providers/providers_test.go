package providers

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
)

// The Node unit tests in node/api/src/services/providers/*.test.js, ported.

type captured struct {
	result   *Result
	err      error
	requests []goldenRequest
	fake     *fakeUpstream
}

// body is the request body of request i as a generic value.
func (c captured) body(t *testing.T, i int) map[string]any {
	t.Helper()
	if i >= len(c.requests) || c.requests[i].Body == nil {
		t.Fatalf("no body for request %d (%d requests)", i, len(c.requests))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(*c.requests[i].Body), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func value(t *testing.T, s string) any {
	t.Helper()
	return parseValue(t, []byte(s))
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func reply(body string) fakeReply {
	return fakeReply{status: 200, body: body}
}

// run makes one call through CreateProvider with the given configuration
// (JSON text, or "" for none) and opts.
func run(t *testing.T, p *Providers, provider, model, conf string, system SystemPrompt, user string, opts *RawOpts, replies ...fakeReply) captured {
	t.Helper()
	fake := &fakeUpstream{queue: replies}
	p.Client = &http.Client{Transport: fake}
	if p.Config == nil {
		p.Config = mapConfig{"virtual_agent_request_timeout": "30"}
	}
	var configuration any
	if conf != "" {
		configuration = value(t, conf)
	}
	call, err := p.CreateProvider(provider, model, "k", configuration)
	if err != nil {
		t.Fatal(err)
	}
	res, err := call(context.Background(), system, user, opts)
	return captured{result: res, err: err, requests: fake.requests, fake: fake}
}

func newProviders() *Providers {
	return &Providers{}
}

func messagesOpt(t *testing.T, msgs string) any {
	return value(t, msgs)
}

// ── anthropic.test.js ──────────────────────────────────────────────────────

const anthropicOK = `{"content":[{"type":"thinking","thinking":""},{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`

func anthropicBody(t *testing.T, model, conf string) map[string]any {
	t.Helper()
	c := run(t, newProviders(), "anthropic", model, conf, TextPrompt("sys"), "hi", &RawOpts{}, reply(anthropicOK))
	if c.err != nil {
		t.Fatal(c.err)
	}
	if c.result.Text != "ok" {
		t.Fatalf("thinking blocks are not text: %v", c.result.Text)
	}
	return c.body(t, 0)
}

func TestAnthropicThinkingByGeneration(t *testing.T) {
	body := anthropicBody(t, "claude-opus-4-6", `{"thinking_effort":"off","temperature":0.4}`)
	if _, ok := body["thinking"]; ok || body["temperature"] != 0.4 {
		t.Errorf("older model, off: %v", body)
	}
	body = anthropicBody(t, "claude-opus-4-6", `{"thinking_effort":"high","temperature":0.4}`)
	if jsonText(body["thinking"]) != `{"type":"adaptive"}` || jsonText(body["output_config"]) != `{"effort":"high"}` || body["temperature"] != nil {
		t.Errorf("older model, on: %v", body)
	}
	body = anthropicBody(t, "claude-opus-4-8", `{"thinking_effort":"off","temperature":0.4}`)
	if _, ok := body["thinking"]; ok || body["temperature"] != nil {
		t.Errorf("Opus 4.8: %v", body)
	}
	for _, model := range []string{"claude-opus-5", "claude-sonnet-5"} {
		body = anthropicBody(t, model, `{"thinking_effort":"off","temperature":0.4}`)
		if jsonText(body["thinking"]) != `{"type":"disabled"}` || body["output_config"] != nil || body["temperature"] != nil {
			t.Errorf("%s off: %v", model, body)
		}
		body = anthropicBody(t, model, `{}`)
		if body["thinking"] != nil || body["output_config"] != nil {
			t.Errorf("%s unset must not disable thinking: %v", model, body)
		}
		body = anthropicBody(t, model, `{"thinking_effort":"xhigh"}`)
		if jsonText(body["thinking"]) != `{"type":"adaptive"}` || jsonText(body["output_config"]) != `{"effort":"xhigh"}` {
			t.Errorf("%s xhigh: %v", model, body)
		}
	}
	for _, model := range []string{"claude-opus-5-5", "claude-fable-5-1", "claude-haiku-5-5"} {
		body = anthropicBody(t, model, `{"thinking_effort":"off","temperature":0.4}`)
		if jsonText(body["thinking"]) != `{"type":"adaptive"}` || jsonText(body["output_config"]) != `{"effort":"low"}` || body["temperature"] != nil {
			t.Errorf("%s stale off: %v", model, body)
		}
		body = anthropicBody(t, model, `{}`)
		if jsonText(body["thinking"]) != `{"type":"adaptive"}` || body["output_config"] != nil {
			t.Errorf("%s unset: %v", model, body)
		}
		body = anthropicBody(t, model, `{"thinking_effort":"max"}`)
		if jsonText(body["output_config"]) != `{"effort":"max"}` {
			t.Errorf("%s max: %v", model, body)
		}
		effort := capability(anthropicModels.Get(model).Capabilities, "thinking_effort")
		if contains(effort.Options, "off") || !contains(effort.Options, effort.Default.(string)) {
			t.Errorf("%s options %v default %v", model, effort.Options, effort.Default)
		}
	}
}

func capability(caps Capabilities, name string) *Capability {
	for i := range caps {
		if caps[i].Name == name {
			return &caps[i]
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestAnthropicRegistryEntries(t *testing.T) {
	for _, m := range anthropicModels {
		if m.APIID == "" || m.ConfigVersion < 1 {
			t.Errorf("%s: apiId %q configVersion %d", m.ID, m.APIID, m.ConfigVersion)
		}
		if m.Pricing.Input == nil || m.Pricing.Output == nil || m.Pricing.CacheWrite == nil || m.Pricing.CacheRead == nil {
			t.Errorf("%s: incomplete pricing", m.ID)
		}
		if e := capability(m.Capabilities, "thinking_effort"); e != nil && !contains(e.Options, e.Default.(string)) {
			t.Errorf("%s: default effort not an option", m.ID)
		}
	}
	for _, id := range []string{"claude-opus-4-8", "claude-opus-5", "claude-opus-5-5", "claude-sonnet-5", "claude-fable-5-1", "claude-opus-4-7", "claude-haiku-5-5"} {
		if capability(anthropicModels.Get(id).Capabilities, "temperature") != nil {
			t.Errorf("%s must not offer temperature", id)
		}
	}
	for id, v := range map[string]int{"claude-opus-4-7": 2, "claude-opus-4-6": 2, "claude-sonnet-4-6": 2, "claude-haiku-4-5": 1} {
		if got := anthropicModels.Get(id).ConfigVersion; got != v {
			t.Errorf("%s configVersion %d, want %d", id, got, v)
		}
	}
}

// historyAfterRounds is a tool loop in the neutral shape.
func historyAfterRounds(rounds int) string {
	msgs := []any{map[string]any{"role": "user", "content": "The court is in session."}}
	for i := 1; i <= rounds; i++ {
		n := jsutil.NumberString(float64(i))
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": "Reading record " + n + ".", "tool_calls": []any{map[string]any{"id": "call_" + n, "type": "function", "function": map[string]any{"name": "read_record", "arguments": `{"n":` + n + `}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "call_" + n, "content": "record " + n + " text"},
		)
	}
	return jsonText(msgs)
}

var cachedSystem = StructuredPrompt("static rules", "")

func anthropicCacheBody(t *testing.T, conf string, opts *RawOpts) map[string]any {
	t.Helper()
	c := run(t, newProviders(), "anthropic", "claude-opus-5-5", conf, cachedSystem, "", opts, reply(anthropicOK))
	if c.err != nil {
		t.Fatal(c.err)
	}
	return c.body(t, 0)
}

func markerPositions(body map[string]any) []string {
	var found []string
	for i, m := range body["messages"].([]any) {
		if blocks, ok := m.(map[string]any)["content"].([]any); ok {
			for j, b := range blocks {
				if _, ok := b.(map[string]any)["cache_control"]; ok {
					found = append(found, jsutil.NumberString(float64(i))+":"+jsutil.NumberString(float64(j)))
				}
			}
		}
	}
	return found
}

func lastMessage(body map[string]any) map[string]any {
	msgs := body["messages"].([]any)
	return msgs[len(msgs)-1].(map[string]any)
}

func TestAnthropicHistoryCacheBreakpoint(t *testing.T) {
	body := anthropicCacheBody(t, `{"cache_prompts":true}`, &RawOpts{Cache: true, Messages: messagesOpt(t, historyAfterRounds(2))})
	last := lastMessage(body)
	blocks := last["content"].([]any)
	if last["role"] != "user" || blocks[len(blocks)-1].(map[string]any)["type"] != "tool_result" {
		t.Errorf("last message: %v", last)
	}
	n := len(body["messages"].([]any))
	if got := markerPositions(body); jsonText(got) != jsonText([]string{jsutil.NumberString(float64(n-1)) + ":" + jsutil.NumberString(float64(len(blocks)-1))}) {
		t.Errorf("markers %v", got)
	}
	if jsonText(body["system"].([]any)[0].(map[string]any)["cache_control"]) != `{"type":"ephemeral"}` {
		t.Errorf("system breakpoint missing")
	}
	if _, ok := body["cache_control"]; ok {
		t.Errorf("top-level marker")
	}

	msgs := strings.TrimSuffix(historyAfterRounds(1), "]") + `,{"role":"user","content":"A new perception."}]`
	body = anthropicCacheBody(t, `{"cache_prompts":true}`, &RawOpts{Cache: true, Messages: messagesOpt(t, msgs), TurnContext: "volatile now"})
	if got := jsonText(lastMessage(body)["content"]); got != `[{"cache_control":{"type":"ephemeral"},"text":"A new perception.","type":"text"},{"text":"volatile now","type":"text"}]` {
		t.Errorf("perception + context: %s", got)
	}

	body = anthropicCacheBody(t, `{"cache_prompts":true}`, &RawOpts{Cache: true, Messages: messagesOpt(t, historyAfterRounds(1)), TurnContext: "volatile now"})
	blocks = lastMessage(body)["content"].([]any)
	if len(blocks) != 2 || blocks[0].(map[string]any)["type"] != "tool_result" || jsonText(blocks[0].(map[string]any)["cache_control"]) != `{"type":"ephemeral"}` || jsonText(blocks[1]) != `{"text":"volatile now","type":"text"}` {
		t.Errorf("tool results + context: %v", blocks)
	}

	body = anthropicCacheBody(t, `{}`, &RawOpts{Cache: true, Messages: messagesOpt(t, `[{"role":"user","content":"A perception."}]`), TurnContext: "volatile now"})
	if len(markerPositions(body)) != 0 || jsonText(body["messages"].([]any)[0].(map[string]any)["content"]) != `[{"text":"A perception.","type":"text"},{"text":"volatile now","type":"text"}]` {
		t.Errorf("caching off: %v", body["messages"])
	}

	body = anthropicCacheBody(t, `{"cache_prompts":true}`, &RawOpts{Cache: true})
	if jsonText(body["messages"]) != `[{"content":"","role":"user"}]` {
		t.Errorf("single message: %v", body["messages"])
	}

	c := run(t, newProviders(), "anthropic", "claude-opus-5-5", `{"cache_prompts":true}`, cachedSystem, "one-off question", nil, reply(anthropicOK))
	if c.err != nil || c.result.Text != "ok" || jsonText(c.body(t, 0)["messages"]) != `[{"content":"one-off question","role":"user"}]` {
		t.Errorf("no opts: %v %v", c.err, c.requests)
	}

	body = anthropicCacheBody(t, `{"cache_prompts":true}`, &RawOpts{Cache: true, Messages: messagesOpt(t, `[{"role":"user","content":""}]`)})
	if len(markerPositions(body)) != 0 {
		t.Errorf("empty text marked")
	}
}

// normalizeHistory strips markers and turns string content into its
// one-block form.
func normalizeHistory(msgs []any) []any {
	out := make([]any, len(msgs))
	for i, m := range msgs {
		mm := m.(map[string]any)
		content := mm["content"]
		if s, ok := content.(string); ok {
			content = []any{map[string]any{"type": "text", "text": s}}
		}
		var blocks []any
		for _, b := range content.([]any) {
			c := map[string]any{}
			for k, v := range b.(map[string]any) {
				if k != "cache_control" {
					c[k] = v
				}
			}
			blocks = append(blocks, c)
		}
		out[i] = map[string]any{"role": mm["role"], "content": blocks}
	}
	return out
}

func TestAnthropicRoundIsPrefixOfNextRound(t *testing.T) {
	roundN := anthropicCacheBody(t, `{"cache_prompts":true}`, &RawOpts{Cache: true, Messages: messagesOpt(t, historyAfterRounds(2)), TurnContext: "ctx N"})
	roundNext := anthropicCacheBody(t, `{"cache_prompts":true}`, &RawOpts{Cache: true, Messages: messagesOpt(t, historyAfterRounds(3)), TurnContext: "ctx N+1"})
	prev := normalizeHistory(roundN["messages"].([]any))
	last := prev[len(prev)-1].(map[string]any)
	var kept []any
	for _, b := range last["content"].([]any) {
		if b.(map[string]any)["text"] != "ctx N" {
			kept = append(kept, b)
		}
	}
	last["content"] = kept
	next := normalizeHistory(roundNext["messages"].([]any))
	if !reflect.DeepEqual(next[:len(prev)], prev) {
		t.Errorf("round N is not a prefix of round N+1\n%s\n%s", jsonText(prev), jsonText(next[:len(prev)]))
	}
}

// ── finish.test.js ─────────────────────────────────────────────────────────

func TestFinishNormalizers(t *testing.T) {
	for in, want := range map[any]string{"stop": FinishStop, "length": FinishLength, "tool_calls": FinishToolCalls, "function_call": FinishToolCalls, "content_filter": FinishContentFilter, "surprise": FinishOther} {
		if got := NormalizeOpenAIChatFinish(in); got != want {
			t.Errorf("chat %v: %s", in, got)
		}
	}
	for _, in := range []any{undefined, nil} {
		if NormalizeOpenAIChatFinish(in) != FinishOther || NormalizeAnthropicStop(in) != FinishOther || NormalizeGoogleFinish(in) != FinishOther {
			t.Errorf("missing signal must be other")
		}
	}
	cases := []struct {
		status, reason any
		tool           bool
		want           string
	}{
		{"incomplete", "max_output_tokens", false, FinishLength},
		{"incomplete", "content_filter", false, FinishContentFilter},
		{"incomplete", "something_else", false, FinishOther},
		{"completed", undefined, false, FinishStop},
		{"completed", undefined, true, FinishToolCalls},
		{"failed", undefined, false, FinishOther},
		{"cancelled", undefined, false, FinishOther},
		{"in_progress", undefined, false, FinishOther},
		{"surprise_status", undefined, false, FinishOther},
		{undefined, undefined, false, FinishOther},
	}
	for _, c := range cases {
		if got := NormalizeResponsesStatus(c.status, c.reason, c.tool); got != c.want {
			t.Errorf("responses %v %v %v: %s", c.status, c.reason, c.tool, got)
		}
	}
	for in, want := range map[string]string{"end_turn": FinishStop, "stop_sequence": FinishStop, "max_tokens": FinishLength, "tool_use": FinishToolCalls, "refusal": FinishContentFilter, "pause_turn": FinishOther} {
		if got := NormalizeAnthropicStop(in); got != want {
			t.Errorf("anthropic %s: %s", in, got)
		}
	}
	for in, want := range map[string]string{"STOP": FinishStop, "MAX_TOKENS": FinishLength, "SAFETY": FinishContentFilter, "RECITATION": FinishContentFilter, "PROHIBITED_CONTENT": FinishContentFilter, "OTHER": FinishOther} {
		if got := NormalizeGoogleFinish(in); got != want {
			t.Errorf("google %s: %s", in, got)
		}
	}
	if !IsTruncated(FinishLength) {
		t.Error("length is truncated")
	}
	for _, r := range []string{FinishStop, FinishToolCalls, FinishContentFilter, FinishOther, ""} {
		if IsTruncated(r) {
			t.Errorf("%q is not truncated", r)
		}
	}
}

// ── google.test.js ─────────────────────────────────────────────────────────

func TestGoogleTruncation(t *testing.T) {
	for reason, want := range map[string]string{"MAX_TOKENS": FinishLength, "STOP": FinishStop} {
		c := run(t, newProviders(), "google", "gemini-2.5-pro", `{}`, TextPrompt("sys"), "hi", &RawOpts{},
			reply(`{"candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":"`+reason+`"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4096}}`))
		if c.err != nil || c.result.FinishReason != want || c.result.Truncated != (want == FinishLength) {
			t.Errorf("%s: %v %+v", reason, c.err, c.result)
		}
	}
}

// LLM-930: Gemini reports thinking tokens apart from the answer and bills
// them at the output rate, so they are part of output_tokens.
func TestGoogleThinkingTokens(t *testing.T) {
	for _, tc := range []struct{ usage, want string }{
		{`{"promptTokenCount":10,"candidatesTokenCount":40,"thoughtsTokenCount":900,"totalTokenCount":950}`, "940"},
		{`{"promptTokenCount":10,"candidatesTokenCount":40,"totalTokenCount":50}`, "40"},
		{`{"promptTokenCount":10,"thoughtsTokenCount":8192,"totalTokenCount":8202}`, "8192"},
	} {
		c := run(t, newProviders(), "google", "gemini-2.5-flash", `{}`, TextPrompt("sys"), "hi", &RawOpts{},
			reply(`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":`+tc.usage+`}`))
		if c.err != nil {
			t.Fatal(c.err)
		}
		if got := jsutil.Marshal(c.result.Usage.OutputTokens); got != tc.want {
			t.Errorf("%s: output_tokens %s, want %s", tc.usage, got, tc.want)
		}
	}
}

// ── index.test.js ──────────────────────────────────────────────────────────

func objectsOf(t *testing.T, s string) []*jsutil.Object {
	var out []*jsutil.Object
	for _, m := range value(t, s).([]any) {
		out = append(out, m.(*jsutil.Object))
	}
	return out
}

func TestAttachTurnContext(t *testing.T) {
	in := objectsOf(t, `[{"role":"user","content":"perception"}]`)
	out, _ := AttachTurnContext(in, "ctx")
	if jsutil.Marshal(objects(out)) != `[{"role":"user","content":"perception\n\nctx"}]` || jsutil.Marshal(objects(in)) != `[{"role":"user","content":"perception"}]` {
		t.Errorf("latest user turn: %s", jsutil.Marshal(objects(out)))
	}
	out, _ = AttachTurnContext(objectsOf(t, `[{"role":"user","content":""}]`), "ctx")
	if jsutil.Marshal(objects(out)) != `[{"role":"user","content":"ctx"}]` {
		t.Errorf("empty user turn: %s", jsutil.Marshal(objects(out)))
	}
	in = objectsOf(t, `[{"role":"user","content":"p"},{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"x","arguments":"{}"}}]},{"role":"tool","tool_call_id":"c1","content":"r"}]`)
	out, _ = AttachTurnContext(in, "ctx")
	if len(out) != 4 || jsutil.Marshal(out[3]) != `{"role":"user","content":"ctx"}` || len(in) != 3 {
		t.Errorf("after tool results: %s", jsutil.Marshal(objects(out)))
	}
	in = objectsOf(t, `[{"role":"user","content":"p"}]`)
	if out, _ := AttachTurnContext(in, ""); &out[0] != &in[0] {
		t.Error("no context must return the same messages")
	}
}

func TestSanitizeOptsTurnContext(t *testing.T) {
	msgs := value(t, `[{"role":"user","content":"p"}]`)
	if o := SanitizeOpts(&RawOpts{Messages: msgs, TurnContext: "ctx"}); o.TurnContext != "ctx" {
		t.Error("turn context with messages")
	}
	if o := SanitizeOpts(&RawOpts{Cache: true, TurnContext: "ctx"}); o.TurnContext != "" {
		t.Error("turn context without messages")
	}
	if o := SanitizeOpts(&RawOpts{Messages: msgs, TurnContext: ""}); o.TurnContext != "" {
		t.Error("empty turn context")
	}
}

func TestCreateProviderRoutesTurnContext(t *testing.T) {
	c := run(t, newProviders(), "openrouter", "deepseek/deepseek-v4-flash", `{}`, TextPrompt("sys"), "", &RawOpts{Messages: value(t, `[{"role":"user","content":"perception"}]`), TurnContext: "ctx"},
		reply(`{"choices":[{"message":{"content":"ok","tool_calls":[]}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`), reply(`{"data":[]}`))
	msgs := c.body(t, 0)["messages"].([]any)
	if jsonText(msgs[len(msgs)-1]) != `{"content":"perception\n\nctx","role":"user"}` {
		t.Errorf("merged: %v", msgs)
	}
	users := 0
	for _, m := range msgs {
		if m.(map[string]any)["role"] == "user" {
			users++
		}
	}
	if users != 1 {
		t.Errorf("extra user turn: %v", msgs)
	}

	c = run(t, newProviders(), "anthropic", "claude-opus-5-5", `{"cache_prompts":true}`, StructuredPrompt("s", ""), "", &RawOpts{Cache: true, Messages: value(t, `[{"role":"user","content":"perception"}]`), TurnContext: "ctx"}, reply(anthropicOK))
	if got := jsonText(c.body(t, 0)["messages"]); got != `[{"content":[{"cache_control":{"type":"ephemeral"},"text":"perception","type":"text"},{"text":"ctx","type":"text"}],"role":"user"}]` {
		t.Errorf("anthropic: %s", got)
	}
}

func TestCreateProviderTimeout(t *testing.T) {
	p := &Providers{Config: mapConfig{"virtual_agent_request_timeout": "1"}}
	slow := fakeReply{status: 200, delay: 5 * time.Second, body: `{}`}
	start := time.Now()
	c := run(t, p, "perplexity", "sonar", `{}`, TextPrompt("sys"), "q", nil, slow)
	if c.err == nil || c.err.Error() != "Provider request timed out after 1s (perplexity/sonar)" {
		t.Errorf("timeout error: %v", c.err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("timeout did not cut the call")
	}

	// The caller's own cancellation is not reported as a timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	call, _ := (&Providers{Config: mapConfig{"virtual_agent_request_timeout": "30"}, Client: &http.Client{Transport: &fakeUpstream{queue: []fakeReply{slow}}}}).CreateProvider("perplexity", "sonar", "k", nil)
	if _, err := call(ctx, TextPrompt("s"), "q", nil); err == nil || strings.Contains(err.Error(), "timed out") {
		t.Errorf("cancelled call: %v", err)
	}
}

// ── openai.test.js ─────────────────────────────────────────────────────────

func pricingText(p *Pricing) string {
	b, _ := json.Marshal(p)
	return string(b)
}

func TestOpenAIRegistry(t *testing.T) {
	for id, want := range map[string]string{
		"gpt-5.6-sol":   `{"input":5,"output":30,"cache_read":0.5}`,
		"gpt-5.6-terra": `{"input":2.5,"output":15,"cache_read":0.25}`,
		"gpt-5.6-luna":  `{"input":1,"output":6,"cache_read":0.1}`,
		"gpt-6.1-sol":   `{"input":2,"output":10,"cache_read":0.1}`,
		"gpt-6.1-terra": `{"input":1,"output":5,"cache_read":0.05}`,
		"gpt-6-luna":    `{"input":0.1,"output":0.5,"cache_read":0.01}`,
	} {
		if got := pricingText(openaiModels.Get(id).Pricing); got != want {
			t.Errorf("%s pricing %s", id, got)
		}
	}
	if openaiModels.Get("gpt-6.1-luna") != nil {
		t.Error("there is no gpt-6.1-luna")
	}
	for _, id := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		m := openaiModels.Get(id)
		if m.ConfigVersion != 2 || strings.Join(capability(m.Capabilities, "reasoning_effort").Options, ",") != "none,low,medium,high,xhigh" || capability(m.Capabilities, "service_tier") == nil {
			t.Errorf("%s capabilities", id)
		}
	}
	all := "none,low,medium,high,xhigh,max"
	for id, options := range map[string]string{"gpt-6.1-sol": "low,medium,high,xhigh,max", "gpt-6.1-terra": all, "gpt-6-luna": all} {
		m := openaiModels.Get(id)
		effort := capability(m.Capabilities, "reasoning_effort")
		if m.ConfigVersion != 3 || effort.Default != "medium" || strings.Join(effort.Options, ",") != options || capability(m.Capabilities, "temperature") != nil || capability(m.Capabilities, "service_tier") == nil {
			t.Errorf("%s capabilities", id)
		}
	}
	// Per-model copies: changing one model's options does not leak.
	terra := capability(openaiModels.Get("gpt-6.1-terra").Capabilities, "reasoning_effort")
	terra.Options = append(terra.Options, "leak")
	if strings.Join(capability(openaiModels.Get("gpt-6-luna").Capabilities, "reasoning_effort").Options, ",") != all {
		t.Error("options leaked between models")
	}
	terra.Options = terra.Options[:len(terra.Options)-1]
}

func responsesCall(t *testing.T, model, conf, usage string) captured {
	t.Helper()
	if usage == "" {
		usage = `{"input_tokens":10,"output_tokens":2}`
	}
	return run(t, newProviders(), "openai", model, conf, TextPrompt("sys"), "hi", &RawOpts{},
		reply(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":`+usage+`}`))
}

func TestOpenAIResponsesEndpoint(t *testing.T) {
	t.Setenv("OPENAI_API_BASE", "")
	for _, id := range []string{"gpt-5.6-sol", "gpt-6.1-sol", "gpt-6.1-terra", "gpt-6-luna"} {
		c := responsesCall(t, id, `{}`, "")
		if c.err != nil || c.requests[0].URL != "https://api.openai.com/v1/responses" || c.result.Text != "ok" {
			t.Errorf("%s: %v %v", id, c.err, c.requests)
		}
	}
	if b := responsesCall(t, "gpt-6-luna", `{"reasoning_effort":"none"}`, "").body(t, 0); jsonText(b["reasoning"]) != `{"effort":"none"}` {
		t.Errorf("gpt-6 none: %v", b["reasoning"])
	}
	if b := responsesCall(t, "gpt-6.1-sol", `{"reasoning_effort":"max"}`, "").body(t, 0); jsonText(b["reasoning"]) != `{"effort":"max"}` {
		t.Errorf("gpt-6 max: %v", b["reasoning"])
	}
	if b := responsesCall(t, "gpt-6.1-terra", `{}`, "").body(t, 0); b["reasoning"] != nil {
		t.Errorf("gpt-6 unset: %v", b["reasoning"])
	}
	if b := responsesCall(t, "gpt-5.6-luna", `{"reasoning_effort":"none"}`, "").body(t, 0); b["reasoning"] != nil {
		t.Errorf("gpt-5 none: %v", b["reasoning"])
	}
}

// LLM-930: the Service Tier control reaches /v1/responses, as on the
// chat-completions path.
func TestOpenAIResponsesServiceTier(t *testing.T) {
	for _, id := range []string{"gpt-5.4-mini", "gpt-5.6-luna", "gpt-6-luna"} {
		if b := responsesCall(t, id, `{"service_tier":"flex"}`, "").body(t, 0); b["service_tier"] != "flex" {
			t.Errorf("%s flex: %v", id, b["service_tier"])
		}
		for _, conf := range []string{`{"service_tier":"auto"}`, `{}`} {
			if b := responsesCall(t, id, conf, "").body(t, 0); b["service_tier"] != nil {
				t.Errorf("%s %s: %v", id, conf, b["service_tier"])
			}
		}
	}
	usage := `{"input_tokens":1000,"output_tokens":100}`
	standard := (1000*1.00 + 100*6) / 1_000_000
	if c := responsesCall(t, "gpt-5.6-luna", `{"service_tier":"flex"}`, usage); c.err != nil || math.Abs(*c.result.Usage.Cost-standard/2) >= 1e-12 {
		t.Errorf("requested flex: cost %v (%v)", c.result.Usage.Cost, c.err)
	}
	if c := responsesCall(t, "gpt-5.6-luna", `{}`, usage); c.err != nil || math.Abs(*c.result.Usage.Cost-standard) >= 1e-12 {
		t.Errorf("unset: cost %v (%v)", c.result.Usage.Cost, c.err)
	}
}

// LLM-930: a call the emit pass skips must not make its tool result look
// paired, or the output goes out alone and OpenAI refuses the request.
func TestOpenAIResponsesPairingSkippedCalls(t *testing.T) {
	msgs := `[{"role":"user","content":"start"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"good","type":"function","function":{"name":"look","arguments":"{}"}},{"id":"wrong-type","type":"custom","function":{"name":"look","arguments":"{}"}},{"id":"no-function","type":"function"}]},` +
		`{"role":"tool","tool_call_id":"good","content":"seen"},{"role":"tool","tool_call_id":"wrong-type","content":"orphan 1"},{"role":"tool","tool_call_id":"no-function","content":"orphan 2"}]`
	c := run(t, newProviders(), "openai", "gpt-5.4", `{}`, TextPrompt("sys"), "hi", &RawOpts{Messages: value(t, msgs)},
		reply(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":10,"output_tokens":2}}`))
	if c.err != nil {
		t.Fatal(c.err)
	}
	var calls, outputs []string
	for _, item := range c.body(t, 0)["input"].([]any) {
		m := item.(map[string]any)
		switch m["type"] {
		case "function_call":
			calls = append(calls, m["call_id"].(string))
		case "function_call_output":
			outputs = append(outputs, m["call_id"].(string))
		}
	}
	if jsonText(calls) != `["good"]` || jsonText(outputs) != `["good"]` {
		t.Errorf("calls %v, outputs %v", calls, outputs)
	}
}

func TestOpenAITruncation(t *testing.T) {
	c := run(t, newProviders(), "openai", "gpt-5.6-sol", `{}`, TextPrompt("sys"), "hi", &RawOpts{},
		reply(`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","content":[{"type":"output_text","text":"partial"}]}],"usage":{"input_tokens":10,"output_tokens":8192}}`))
	if c.err != nil || c.result.FinishReason != FinishLength || !c.result.Truncated {
		t.Errorf("responses incomplete: %v %+v", c.err, c.result)
	}
	c = responsesCall(t, "gpt-5.6-sol", `{}`, "")
	if c.result.FinishReason != FinishStop || c.result.Truncated {
		t.Errorf("responses completed: %+v", c.result)
	}
	c = run(t, newProviders(), "openai", "gpt-4o-mini", `{}`, TextPrompt("sys"), "hi", &RawOpts{},
		reply(`{"choices":[{"message":{"content":"partial","tool_calls":[]},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":4096}}`))
	if c.err != nil || c.result.FinishReason != FinishLength || !c.result.Truncated {
		t.Errorf("chat length: %v %+v", c.err, c.result)
	}
}

func TestOpenAILongContextCost(t *testing.T) {
	check := func(model, usage string, want float64) {
		c := responsesCall(t, model, `{}`, usage)
		if c.err != nil || math.Abs(*c.result.Usage.Cost-want) >= 1e-12 {
			t.Errorf("%s %s: cost %v, want %v (%v)", model, usage, c.result.Usage.Cost, want, c.err)
		}
	}
	check("gpt-6.1-sol", `{"input_tokens":272000,"input_tokens_details":{"cached_tokens":200000},"output_tokens":10000}`, (72000*2.00+200000*0.10+10000*10)/1_000_000)
	check("gpt-6.1-terra", `{"input_tokens":300000,"input_tokens_details":{"cached_tokens":100000},"output_tokens":10000}`, (200000*1.00*2+100000*0.05*2+10000*5*1.5)/1_000_000)
	check("gpt-5.6-luna", `{"input_tokens":300000,"output_tokens":10000}`, (300000*1.00+10000*6)/1_000_000)
}

// ── openrouter.test.js ─────────────────────────────────────────────────────

const openrouterOK = `{"choices":[{"message":{"content":"ok","tool_calls":[]}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`
const emptyCatalog = `{"data":[]}`
const pricedCatalog = `{"data":[{"id":"deepseek/deepseek-v4-flash","pricing":{"prompt":"0.00001","completion":"0.00001"}}]}`

func openrouterBody(t *testing.T, conf string) map[string]any {
	t.Helper()
	c := run(t, newProviders(), "openrouter", "deepseek/deepseek-v4-flash", conf, TextPrompt("sys"), "hi", &RawOpts{}, reply(openrouterOK), reply(emptyCatalog))
	if c.err != nil {
		t.Fatal(c.err)
	}
	return c.body(t, 0)
}

func TestOpenRouterProviderRouting(t *testing.T) {
	if got := jsonText(openrouterBody(t, `{"provider":{"order":["deepinfra"],"allow_fallbacks":false}}`)["provider"]); got != `{"allow_fallbacks":false,"order":["deepinfra"]}` {
		t.Errorf("routing: %s", got)
	}
	for _, conf := range []string{`{}`, `{"provider":"deepinfra"}`, `{"provider":["deepinfra"]}`} {
		if _, ok := openrouterBody(t, conf)["provider"]; ok {
			t.Errorf("%s: provider sent", conf)
		}
	}
}

func TestOpenRouterTruncation(t *testing.T) {
	for reason, want := range map[string]string{"length": FinishLength, "stop": FinishStop} {
		c := run(t, newProviders(), "openrouter", "google/gemini-2.5-pro", `{}`, TextPrompt("sys"), "hi", &RawOpts{},
			reply(`{"choices":[{"message":{"content":"partial","tool_calls":[]},"finish_reason":"`+reason+`"}],"usage":{"prompt_tokens":10,"completion_tokens":4096}}`), reply(emptyCatalog))
		if c.err != nil || c.result.FinishReason != want || c.result.Truncated != (want == FinishLength) {
			t.Errorf("%s: %v %+v", reason, c.err, c.result)
		}
	}
}

func openrouterCompletion(t *testing.T, fields string) captured {
	t.Helper()
	body := `{"choices":[{"message":{"content":"ok","tool_calls":[]},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}` + fields + `}`
	c := run(t, newProviders(), "openrouter", "deepseek/deepseek-v4-flash", `{}`, TextPrompt("sys"), "hi", &RawOpts{}, reply(body), reply(pricedCatalog))
	if c.err != nil {
		t.Fatal(c.err)
	}
	return c
}

func TestOpenRouterBilledCost(t *testing.T) {
	// A repeated key: the last value wins, as in JSON.parse.
	c := openrouterCompletion(t, `,"provider":"Baidu","usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.000001365}`)
	if *c.result.Usage.Cost != 0.000001365 || *c.result.Usage.ServedBy != "Baidu" {
		t.Errorf("billed: %v %v", *c.result.Usage.Cost, c.result.Usage.ServedBy)
	}
	if c := openrouterCompletion(t, `,"provider":"Baidu"`); *c.result.Usage.Cost != 0.00012 {
		t.Errorf("catalog fallback: %v", *c.result.Usage.Cost)
	}
	for _, bad := range []string{`"not-a-number"`, `-1`, `null`} {
		if c := openrouterCompletion(t, `,"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":`+bad+`}`); *c.result.Usage.Cost != 0.00012 {
			t.Errorf("bad cost %s: %v", bad, *c.result.Usage.Cost)
		}
	}
	if c := openrouterCompletion(t, `,"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0}`); *c.result.Usage.Cost != 0 {
		t.Errorf("zero cost: %v", *c.result.Usage.Cost)
	}
}

func TestOpenRouterServedBy(t *testing.T) {
	for _, bad := range []string{``, `,"provider":""`, `,"provider":"   "`, `,"provider":"\t\n"`, `,"provider":42`} {
		if c := openrouterCompletion(t, bad); c.result.Usage.ServedBy != nil {
			t.Errorf("%s: served_by %q", bad, *c.result.Usage.ServedBy)
		}
	}
	if c := openrouterCompletion(t, `,"provider":"  Baidu \n"`); *c.result.Usage.ServedBy != "Baidu" {
		t.Errorf("trimmed: %q", *c.result.Usage.ServedBy)
	}
	if c := openrouterCompletion(t, `,"provider":"`+strings.Repeat("x", 500)+`"`); len(*c.result.Usage.ServedBy) != 100 {
		t.Errorf("capped: %d", len(*c.result.Usage.ServedBy))
	}
}

func TestOpenRouterReasoning(t *testing.T) {
	if got := jsonText(openrouterBody(t, `{"thinking_effort":"off"}`)["reasoning"]); got != `{"effort":"none"}` {
		t.Errorf("off: %s", got)
	}
	for _, level := range []string{"low", "medium", "high", "max"} {
		if got := jsonText(openrouterBody(t, `{"thinking_effort":"`+level+`"}`)["reasoning"]); got != `{"effort":"`+level+`"}` {
			t.Errorf("%s: %s", level, got)
		}
	}
	for _, conf := range []string{`{}`, `{"thinking_effort":"none"}`, `{"thinking_effort":"xhigh"}`, `{"thinking_effort":""}`, `{"thinking_effort":"constructor"}`, `{"thinking_effort":"toString"}`, `{"thinking_effort":3}`} {
		if _, ok := openrouterBody(t, conf)["reasoning"]; ok {
			t.Errorf("%s: reasoning sent", conf)
		}
	}
}

// lineLogger keeps the log lines.
type lineLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *lineLogger) Log(subsystem, action string, details logger.Fields) {
	b, _ := json.Marshal(details)
	l.mu.Lock()
	l.lines = append(l.lines, subsystem+" "+action+": "+string(b))
	l.mu.Unlock()
}

func TestOpenRouterReasoningCharsLogged(t *testing.T) {
	log := &lineLogger{}
	p := &Providers{Log: log}
	c := run(t, p, "openrouter", "deepseek/deepseek-v4-flash", `{"thinking_effort":"high"}`, TextPrompt("sys"), "hi", &RawOpts{},
		reply(`{"choices":[{"message":{"content":"ok","tool_calls":[],"reasoning":"`+strings.Repeat("x", 1234)+`"}}],"usage":{"prompt_tokens":10,"completion_tokens":400}}`), reply(emptyCatalog))
	var line string
	for _, l := range log.lines {
		if strings.Contains(l, "api-response") {
			line = l
		}
	}
	if !strings.Contains(line, `"reasoning_chars":1234`) {
		t.Errorf("api-response line: %q", line)
	}
	if c.result.Text != "ok" || c.result.Usage.OutputTokens != json.Number("400") {
		t.Errorf("result: %+v", c.result)
	}
}

// ── perplexity.test.js ─────────────────────────────────────────────────────

func sonar(t *testing.T, extra, conf string) string {
	t.Helper()
	c := run(t, newProviders(), "perplexity", "sonar", conf, TextPrompt("sys"), "q", &RawOpts{},
		reply(`{"choices":[{"message":{"content":"answer body"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}`+extra+`}`))
	if c.err != nil {
		t.Fatal(c.err)
	}
	return c.result.Text.(string)
}

func TestPerplexitySources(t *testing.T) {
	text := sonar(t, `,"search_results":[{"title":"First Source","url":"https://a.example/1","date":"2026-07-01"},{"title":"Second Source","url":"https://b.example/2","date":"2026-07-02"}]`, `{}`)
	if !strings.Contains(text, "\n\nSources:\n") || !strings.Contains(text, "[1] First Source — https://a.example/1 (2026-07-01)") || !strings.Contains(text, "[2] Second Source — https://b.example/2 (2026-07-02)") {
		t.Errorf("search_results: %q", text)
	}
	text = sonar(t, `,"search_results":[{"url":"https://a.example/1"}]`, `{}`)
	if !strings.Contains(text, "[1] https://a.example/1") || strings.Contains(text, " — ") || strings.Contains(text, "()") {
		t.Errorf("bare url: %q", text)
	}
	text = sonar(t, `,"citations":["https://a.example/1","https://b.example/2"]`, `{}`)
	if !strings.Contains(text, "[1] https://a.example/1") || !strings.Contains(text, "[2] https://b.example/2") {
		t.Errorf("legacy: %q", text)
	}
	text = sonar(t, `,"search_results":[{"title":"New","url":"https://new.example","date":"2026-07-03"}],"citations":["https://old.example"]`, `{}`)
	if !strings.Contains(text, "new.example") || strings.Contains(text, "old.example") {
		t.Errorf("precedence: %q", text)
	}
	if text = sonar(t, ``, `{}`); text != "answer body" {
		t.Errorf("no sources: %q", text)
	}
	if text = sonar(t, `,"search_results":[{"title":"X","url":"https://x.example","date":"2026-07-01"}]`, `{"return_citations":false}`); text != "answer body" {
		t.Errorf("citations off: %q", text)
	}
	c := run(t, newProviders(), "perplexity", "sonar", `{}`, TextPrompt("sys"), "q", &RawOpts{},
		reply(`{"choices":[{"message":{"content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":4096}}`))
	if c.result.FinishReason != FinishLength || !c.result.Truncated {
		t.Errorf("length: %+v", c.result)
	}
}

// ── api-urls.test.js ───────────────────────────────────────────────────────

func TestAPIURLs(t *testing.T) {
	const base = "http://127.0.0.1:9100/fake/"
	cases := []struct {
		variable, provider, model string
		want                      []string
	}{
		{"ANTHROPIC_API_BASE", "anthropic", "claude-opus-5-5", []string{"https://api.anthropic.com/v1/messages"}},
		{"OPENAI_API_BASE", "openai", "gpt-5.5", []string{"https://api.openai.com/v1/responses"}},
		{"OPENAI_API_BASE", "openai", "gpt-4.1", []string{"https://api.openai.com/v1/chat/completions"}},
		{"GOOGLE_API_BASE", "google", "gemini-2.5-flash", []string{"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent"}},
		{"OPENROUTER_API_BASE", "openrouter", "deepseek/deepseek-v4-flash", []string{"https://openrouter.ai/api/v1/chat/completions", "https://openrouter.ai/api/v1/models"}},
		{"PERPLEXITY_API_BASE", "perplexity", "sonar", []string{"https://api.perplexity.ai/chat/completions"}},
		{"XAI_API_BASE", "xai", "grok-4.20-reasoning", []string{"https://api.x.ai/v1/responses"}},
	}
	urls := func(provider, model string) []string {
		replies := []fakeReply{{status: 418, body: "stub"}}
		if provider == "openrouter" {
			replies = []fakeReply{reply(openrouterOK), {status: 418, body: "stub"}}
		}
		c := run(t, newProviders(), provider, model, `{}`, TextPrompt("sys"), "hi", &RawOpts{}, replies...)
		var out []string
		for _, r := range c.requests {
			out = append(out, r.URL)
		}
		return out
	}
	for _, c := range cases {
		for name := range apiBaseNames {
			t.Setenv(name, "")
		}
		if got := urls(c.provider, c.model); strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s default: %v", c.model, got)
		}
		t.Setenv(c.variable, base)
		var want []string
		for _, u := range c.want {
			want = append(want, "http://127.0.0.1:9100/fake"+u[strings.Index(u[8:], "/")+8:])
		}
		if got := urls(c.provider, c.model); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s override: %v, want %v", c.model, got, want)
		}
	}
	for name := range apiBaseNames {
		t.Setenv(name, "")
	}
	t.Setenv("XAI_API_BASE", base)
	if got := urls("openai", "gpt-5.5"); strings.Join(got, " ") != "https://api.openai.com/v1/responses" {
		t.Errorf("one variable moved another upstream: %v", got)
	}
}

var apiBaseNames = map[string]bool{"OPENAI_API_BASE": true, "ANTHROPIC_API_BASE": true, "GOOGLE_API_BASE": true, "OPENROUTER_API_BASE": true, "PERPLEXITY_API_BASE": true, "XAI_API_BASE": true}

// ── Go-side checks ─────────────────────────────────────────────────────────

func TestEncryptDecryptRoundTrip(t *testing.T) {
	p := &Providers{Config: mapConfig{"virtual_agent_encryption_key": "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"}}
	for _, key := range []string{"sk-ant-api03-xyz", "", "é😀"} {
		enc, err := p.EncryptAPIKey(key)
		if err != nil {
			t.Fatal(err)
		}
		if parts := strings.Split(enc, ":"); len(parts) != 3 || len(parts[0]) != 24 || len(parts[1]) != 32 {
			t.Errorf("format %q", enc)
		}
		if got, err := p.DecryptAPIKey(enc); err != nil || got != key {
			t.Errorf("round trip %q: %q %v", key, got, err)
		}
	}
	if _, err := (&Providers{Config: mapConfig{"virtual_agent_encryption_key": ""}}).EncryptAPIKey("x"); err == nil || err.Error() != "virtual_agent_encryption_key not configured" {
		t.Errorf("unset key: %v", err)
	}
}

func TestRequestBodyKeepsJavaScriptText(t *testing.T) {
	// U+2028 stays raw and < > & are not escaped, as JSON.stringify writes
	// them; numbers keep their JavaScript form.
	c := run(t, newProviders(), "perplexity", "sonar", `{"temperature":"1e2","max_tokens":1.50}`, TextPrompt("a b <tag> & \"q\""), "x", nil,
		reply(`{"choices":[{"message":{"content":"ok"}}]}`))
	if c.err != nil {
		t.Fatal(c.err)
	}
	want := `{"model":"sonar","messages":[{"role":"system","content":"a` + " " + `b <tag> & \"q\""},{"role":"user","content":"x"}],"max_tokens":1.5,"temperature":100}`
	if got := *c.requests[0].Body; got != want {
		t.Errorf("body\n got %s\nwant %s", got, want)
	}
}
