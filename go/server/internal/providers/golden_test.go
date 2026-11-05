package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// goldens is testdata/goldens.json, written by testdata/goldens.js from the
// Node adapters. Rerun the script only when the Node side changes.
type goldens struct {
	Base           string         `json:"base"`
	Calls          []goldenCall   `json:"calls"`
	Registry       string         `json:"registry"`
	Lookups        []goldenLookup `json:"lookups"`
	Costs          []goldenCost   `json:"costs"`
	Formats        []goldenFormat `json:"formats"`
	CatalogFormats []struct {
		Model string `json:"model"`
		Out   string `json:"out"`
	} `json:"catalog_formats"`
	CatalogEntries []string `json:"catalog_entries"`
	Coerce         []struct {
		Schema string `json:"schema"`
		Input  string `json:"input"`
		Out    string `json:"out"`
	} `json:"coerce"`
	AsNumber []struct {
		In  string  `json:"in"`
		Out outcome `json:"out"`
	} `json:"as_number"`
	Sanitize []struct {
		In  string  `json:"in"`
		Out outcome `json:"out"`
	} `json:"sanitize"`
	Attach []struct {
		Messages string  `json:"messages"`
		Context  string  `json:"context"`
		Out      outcome `json:"out"`
	} `json:"attach"`
	Finishes []struct {
		In        string `json:"in"`
		Chat      string `json:"chat"`
		Anthropic string `json:"anthropic"`
		Google    string `json:"google"`
	} `json:"finishes"`
	Statuses []struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
		Tool   bool   `json:"tool"`
		Out    string `json:"out"`
	} `json:"statuses"`
	EncKey  string `json:"enc_key"`
	Decrypt []struct {
		Key *string `json:"key"`
		In  string  `json:"in"`
		Out outcome `json:"out"`
	} `json:"decrypt"`
	ParseFloat []struct {
		In  string `json:"in"`
		Out string `json:"out"`
		NaN bool   `json:"nan"`
	} `json:"parse_float"`
}

// outcome is a function's JSON.stringify'd return value, or its error.
type outcome struct {
	Out   *string `json:"out"`
	Error *string `json:"error"`
}

type goldenCall struct {
	Case struct {
		Name      string          `json:"name"`
		Provider  string          `json:"provider"`
		Model     string          `json:"model"`
		APIKey    string          `json:"apiKey"`
		Conf      json.RawMessage `json:"conf"`
		HasConf   bool            `json:"has_conf"`
		System    json.RawMessage `json:"system"`
		User      string          `json:"user"`
		Opts      json.RawMessage `json:"opts"`
		Config    json.RawMessage `json:"config"`
		Responses []struct {
			Status int    `json:"status"`
			Delay  int    `json:"delay"`
			Body   string `json:"body"`
		} `json:"responses"`
	} `json:"case"`
	Requests []goldenRequest `json:"requests"`
	Result   *string         `json:"result"`
	Error    *struct {
		Message string `json:"message"`
		Status  *int   `json:"status"`
	} `json:"error"`
}

type goldenRequest struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers [][2]string `json:"headers"`
	Body    *string     `json:"body"`
}

type goldenLookup struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Defaults outcome `json:"defaults"`
	Version  outcome `json:"version"`
	Pricing  outcome `json:"pricing"`
}

type goldenCost struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Usage    string  `json:"usage"`
	CostNaN  bool    `json:"cost_nan"`
	Out      outcome `json:"out"`
}

type goldenFormat struct {
	Provider string          `json:"provider"`
	Model    string          `json:"model"`
	Conf     json.RawMessage `json:"conf"`
	Out      outcome         `json:"out"`
}

func loadGoldens(t *testing.T) goldens {
	t.Helper()
	data, err := os.ReadFile("testdata/goldens.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldens
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func parseValue(t *testing.T, raw []byte) any {
	t.Helper()
	v, err := jsutil.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return v
}

// mapConfig is the config table.
type mapConfig map[string]string

func (m mapConfig) Get(key string) (string, error) {
	v, ok := m[key]
	if !ok {
		return "", errors.New(`Config key "` + key + `" not found. Was config.init() called?`)
	}
	return v, nil
}

// fakeUpstream records each request and answers from a queue, as the
// script's fetch stub does.
type fakeUpstream struct {
	mu       sync.Mutex
	queue    []fakeReply
	requests []goldenRequest
}

type fakeReply struct {
	status int
	delay  time.Duration
	body   string
}

func (f *fakeUpstream) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := goldenRequest{Method: req.Method, URL: req.URL.String()}
	var names []string
	for k := range req.Header {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	for _, k := range names {
		rec.Headers = append(rec.Headers, [2]string{strings.ToLower(k), strings.Join(req.Header[k], ", ")})
	}
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		s := string(b)
		rec.Body = &s
	}
	f.mu.Lock()
	f.requests = append(f.requests, rec)
	reply := fakeReply{status: 599, body: "no reply queued"}
	if len(f.queue) > 0 {
		reply, f.queue = f.queue[0], f.queue[1:]
	}
	f.mu.Unlock()
	if reply.delay > 0 {
		select {
		case <-time.After(reply.delay):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	return &http.Response{
		StatusCode: reply.status,
		Body:       io.NopCloser(strings.NewReader(reply.body)),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

var baseConfig = func(g goldens) mapConfig {
	return mapConfig{"virtual_agent_request_timeout": "30", "virtual_agent_encryption_key": g.EncKey}
}

func setBases(t *testing.T, base string) {
	for name := range map[string]bool{"OPENAI_API_BASE": true, "ANTHROPIC_API_BASE": true, "GOOGLE_API_BASE": true, "OPENROUTER_API_BASE": true, "PERPLEXITY_API_BASE": true, "XAI_API_BASE": true} {
		t.Setenv(name, base)
	}
}

// rawOpts is how the script's opts reach the provider: an object's five
// fields, anything else no options.
func rawOpts(v any) *RawOpts {
	o, ok := v.(*jsutil.Object)
	if !ok {
		return nil
	}
	return &RawOpts{Cache: o.Value("cache"), Stop: o.Value("stop"), Tools: o.Value("tools"), Messages: o.Value("messages"), TurnContext: o.Value("turnContext")}
}

func TestCallsMatchNode(t *testing.T) {
	g := loadGoldens(t)
	setBases(t, g.Base)
	for _, gc := range g.Calls {
		c := gc.Case
		t.Run(c.Name, func(t *testing.T) {
			config := baseConfig(g)
			if len(c.Config) > 0 && string(c.Config) != "null" {
				var extra map[string]string
				if err := json.Unmarshal(c.Config, &extra); err != nil {
					t.Fatal(err)
				}
				if len(extra) == 0 {
					config = mapConfig{}
				}
				for k, v := range extra {
					config[k] = v
				}
			}
			fake := &fakeUpstream{}
			for _, r := range c.Responses {
				fake.queue = append(fake.queue, fakeReply{status: r.Status, delay: time.Duration(r.Delay) * time.Millisecond, body: r.Body})
			}
			p := &Providers{Config: config, Client: &http.Client{Transport: fake}}

			var conf any
			if c.HasConf {
				conf = parseValue(t, c.Conf)
			}
			var system SystemPrompt
			switch s := parseValue(t, c.System).(type) {
			case string:
				system = TextPrompt(s)
			case *jsutil.Object:
				system = StructuredPrompt(s.Value("static").(string), s.Value("dynamic").(string))
			}
			opts := rawOpts(parseValue(t, c.Opts))

			var res *Result
			call, err := p.CreateProvider(c.Provider, c.Model, c.APIKey, conf)
			if err == nil {
				res, err = call(context.Background(), system, c.User, opts)
			}

			if gc.Error != nil {
				if err == nil {
					t.Fatalf("want error %q, got result", gc.Error.Message)
				}
				if err.Error() != gc.Error.Message {
					t.Errorf("error\n got %q\nwant %q", err.Error(), gc.Error.Message)
				}
				var apiErr *APIError
				if gc.Error.Status != nil {
					if !errors.As(err, &apiErr) || apiErr.Status != *gc.Error.Status {
						t.Errorf("error status: got %v, want %d", err, *gc.Error.Status)
					}
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				got, _ := res.MarshalJSON()
				if string(got) != *gc.Result {
					t.Errorf("result\n got %s\nwant %s", got, *gc.Result)
				}
			}

			if len(fake.requests) != len(gc.Requests) {
				t.Fatalf("requests: got %d, want %d\n got %+v", len(fake.requests), len(gc.Requests), fake.requests)
			}
			for i, want := range gc.Requests {
				got := fake.requests[i]
				if got.Method != want.Method || got.URL != want.URL {
					t.Errorf("request %d: got %s %s, want %s %s", i, got.Method, got.URL, want.Method, want.URL)
				}
				if !equalHeaders(got.Headers, want.Headers) {
					t.Errorf("request %d headers\n got %v\nwant %v", i, got.Headers, want.Headers)
				}
				if (got.Body == nil) != (want.Body == nil) || (got.Body != nil && *got.Body != *want.Body) {
					t.Errorf("request %d body\n got %s\nwant %s", i, deref(got.Body), deref(want.Body))
				}
			}
		})
	}
}

func equalHeaders(a, b [][2]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func checkOutcome(t *testing.T, what string, got any, err error, want outcome) {
	t.Helper()
	if want.Error != nil {
		if err == nil || err.Error() != *want.Error {
			t.Errorf("%s: got %v, want error %q", what, err, *want.Error)
		}
		return
	}
	if err != nil {
		t.Errorf("%s: unexpected error %v", what, err)
		return
	}
	var text string
	switch v := got.(type) {
	case string:
		text = v
	default:
		b, mErr := jsonutil.Marshal(v)
		if mErr != nil {
			t.Fatalf("%s: %v", what, mErr)
		}
		text = string(b)
	}
	wantText := "null"
	if want.Out != nil {
		wantText = *want.Out
	}
	if text != wantText {
		t.Errorf("%s\n got %s\nwant %s", what, text, wantText)
	}
}

func TestRegistryMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	if got := string(Registry()); got != g.Registry {
		t.Errorf("registry differs\n got %s\nwant %s", got, g.Registry)
	}
}

func TestLookupsMatchNode(t *testing.T) {
	g := loadGoldens(t)
	for _, l := range g.Lookups {
		what := l.Provider + "/" + l.Model
		defaults, err := GetDefaultConfiguration(l.Provider, l.Model)
		var dv any
		if defaults != nil {
			dv = jsutil.Marshal(defaults)
		}
		checkOutcome(t, what+" defaults", dv, err, l.Defaults)

		version, err := GetModelConfigVersion(l.Provider, l.Model)
		var vv any = "null"
		if version != 0 {
			vv = jsutil.NumberString(float64(version))
		}
		checkOutcome(t, what+" version", vv, err, l.Version)

		pricing, err := GetModelPricing(l.Provider, l.Model)
		var pv any = "null"
		if pricing != nil {
			pv = pricing
		}
		checkOutcome(t, what+" pricing", pv, err, l.Pricing)
	}
}

func usageFromJSON(t *testing.T, raw string, costNaN bool) Usage {
	o := parseValue(t, []byte(raw)).(*jsutil.Object)
	field := func(k string) any {
		v, ok := o.Get(k)
		if !ok {
			return nil
		}
		return v
	}
	u := Usage{
		InputTokens:              field("input_tokens"),
		OutputTokens:             field("output_tokens"),
		CacheCreationInputTokens: field("cache_creation_input_tokens"),
		CacheReadInputTokens:     field("cache_read_input_tokens"),
	}
	if costNaN {
		n := math.NaN()
		u.Cost = &n
	} else if c, ok := jsutil.AsFloat(field("cost")); ok {
		u.Cost = &c
	}
	return u
}

func TestCostsMatchNode(t *testing.T) {
	g := loadGoldens(t)
	for _, c := range g.Costs {
		cost, err := CalculateCost(c.Provider, c.Model, usageFromJSON(t, c.Usage, c.CostNaN))
		checkOutcome(t, c.Provider+"/"+c.Model+" "+c.Usage, jsutil.Marshal(cost), err, c.Out)
	}
}

func TestFormatPricingMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	p := &Providers{}
	for _, f := range g.Formats {
		var conf any
		if string(f.Conf) != "null" {
			conf = parseValue(t, f.Conf)
		}
		got, err := p.FormatPricing(f.Provider, f.Model, conf)
		var v any
		if err == nil {
			v = jsutil.Marshal(got)
		}
		checkOutcome(t, f.Provider+"/"+f.Model+" "+string(f.Conf), v, err, f.Out)
	}
}

func TestCatalogMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	setBases(t, g.Base)
	var reply string
	for _, c := range g.Calls {
		for _, r := range c.Case.Responses {
			if strings.Contains(r.Body, "x/no-pricing") {
				reply = r.Body
			}
		}
	}
	fake := &fakeUpstream{queue: []fakeReply{{status: 200, body: reply}}}
	p := &Providers{Client: &http.Client{Transport: fake}}
	catalog := p.FetchCatalog(context.Background())
	var entries []string
	for _, e := range catalog.Entries() {
		entry := jsutil.NewObject()
		entry.Set("input", e.Input)
		entry.Set("output", e.Output)
		if e.CacheRead != nil {
			entry.Set("cache_read", *e.CacheRead)
		} else {
			entry.Set("cache_read", nil)
		}
		entry.Set("context_length", e.ContextLength)
		entry.Set("name", e.Name)
		o := jsutil.NewObject()
		o.Set("id", e.ID)
		o.Set("entry", entry)
		entries = append(entries, jsutil.Marshal(o))
	}
	if strings.Join(entries, "\n") != strings.Join(g.CatalogEntries, "\n") {
		t.Errorf("catalog\n got %v\nwant %v", entries, g.CatalogEntries)
	}
	for _, f := range g.CatalogFormats {
		got, _ := p.FormatPricing("openrouter", f.Model, nil)
		if got != f.Out {
			t.Errorf("format %s: got %q, want %q", f.Model, got, f.Out)
		}
	}
	// A second fetch within the TTL is served from the cache.
	p.FetchCatalog(context.Background())
	if len(fake.requests) != 1 {
		t.Errorf("catalog fetched %d times, want 1", len(fake.requests))
	}
}

func TestCoerceMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	for _, c := range g.Coerce {
		got := jsutil.Marshal(CoerceToolArgs(parseValue(t, []byte(c.Input)), parseValue(t, []byte(c.Schema))))
		if got != c.Out {
			t.Errorf("coerce %s against %s\n got %s\nwant %s", c.Input, c.Schema, got, c.Out)
		}
	}
}

func TestAsNumberMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	for _, c := range g.AsNumber {
		n, ok, err := asNumber(parseValue(t, []byte(c.In)))
		var v any
		if ok {
			v = jsutil.Marshal(n)
		}
		checkOutcome(t, "asNumber "+c.In, v, err, c.Out)
	}
}

func TestSanitizeMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	for _, c := range g.Sanitize {
		opts := SanitizeOpts(rawOpts(parseValue(t, []byte(c.In))))
		var v any
		if opts != nil {
			o := jsutil.NewObject()
			if opts.Cache {
				o.Set("cache", true)
			}
			if len(opts.Stop) > 0 {
				o.Set("stop", stringsToValues(opts.Stop, len(opts.Stop)))
			}
			if len(opts.Tools) > 0 {
				o.Set("tools", objects(opts.Tools))
			}
			if len(opts.Messages) > 0 {
				o.Set("messages", objects(opts.Messages))
			}
			if opts.TurnContext != "" {
				o.Set("turnContext", opts.TurnContext)
			}
			v = jsutil.Marshal(o)
		}
		checkOutcome(t, "sanitize "+c.In, v, nil, c.Out)
	}
}

func objects(list []*jsutil.Object) []any {
	out := make([]any, len(list))
	for i, o := range list {
		out[i] = o
	}
	return out
}

func TestAttachTurnContextMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	for _, c := range g.Attach {
		var msgs []*jsutil.Object
		for _, m := range parseValue(t, []byte(c.Messages)).([]any) {
			msgs = append(msgs, m.(*jsutil.Object))
		}
		out, err := AttachTurnContext(msgs, c.Context)
		var v any
		if err == nil {
			v = jsutil.Marshal(objects(out))
		}
		checkOutcome(t, "attach "+c.Messages, v, err, c.Out)
		if again := jsutil.Marshal(objects(msgs)); again != c.Messages {
			t.Errorf("attach changed its input: %s", again)
		}
	}
}

func TestFinishMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	for _, f := range g.Finishes {
		v := parseValue(t, []byte(f.In))
		if got := NormalizeOpenAIChatFinish(v); got != f.Chat {
			t.Errorf("chat %s: got %s, want %s", f.In, got, f.Chat)
		}
		if got := NormalizeAnthropicStop(v); got != f.Anthropic {
			t.Errorf("anthropic %s: got %s, want %s", f.In, got, f.Anthropic)
		}
		if got := NormalizeGoogleFinish(v); got != f.Google {
			t.Errorf("google %s: got %s, want %s", f.In, got, f.Google)
		}
	}
	for _, s := range g.Statuses {
		got := NormalizeResponsesStatus(parseValue(t, []byte(s.Status)), parseValue(t, []byte(s.Reason)), s.Tool)
		if got != s.Out {
			t.Errorf("status %s %s %v: got %s, want %s", s.Status, s.Reason, s.Tool, got, s.Out)
		}
	}
}

func TestDecryptMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	for _, d := range g.Decrypt {
		config := mapConfig{}
		if d.Key != nil {
			config["virtual_agent_encryption_key"] = *d.Key
		}
		p := &Providers{Config: config}
		got, err := p.DecryptAPIKey(d.In)
		var v any
		if err == nil {
			v = jsutil.Marshal(got)
		}
		checkOutcome(t, "decrypt "+d.In+" key "+deref(d.Key), v, err, d.Out)
	}
}

func TestParseFloatMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	for _, c := range g.ParseFloat {
		f, ok := jsutil.ParseFloat(c.In)
		if c.NaN {
			if ok || !math.IsNaN(f) {
				t.Errorf("parseFloat(%q) = %v, want NaN", c.In, f)
			}
			continue
		}
		if got := jsutil.Marshal(f); got != c.Out {
			t.Errorf("parseFloat(%q) = %s, want %s", c.In, got, c.Out)
		}
	}
}
