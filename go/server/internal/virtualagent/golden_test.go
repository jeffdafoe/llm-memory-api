package virtualagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
)

// The goldens are what Node's virtual-agent.js did for each scenario, with
// the same stubs this file builds (testdata/goldens.js). Each scenario is
// replayed against the Go service and the recordings compared: the queries
// (kind and parameters as node-postgres prepared them), log lines,
// broadcasts, notes, provider calls, timers and the outcome.

type outcome struct {
	OK    *string `json:"ok"`
	Error *string `json:"error"`
}

type queryRec struct {
	Kind   string    `json:"kind"`
	Params []*string `json:"params"`
	// Timer is the activity spinner's off-switch, made after a timer.
	Timer bool `json:"timer,omitempty"`
}

type logRec struct {
	Subsystem string `json:"subsystem"`
	Action    string `json:"action"`
	Details   string `json:"details"`
}

type eventRec struct {
	Event string `json:"event"`
	Data  string `json:"data"`
}

type noteRec struct {
	Namespace string          `json:"namespace"`
	Title     string          `json:"title"`
	Content   json.RawMessage `json:"content"`
	Slug      string          `json:"slug"`
	CreatedBy string          `json:"created_by"`
}

type createdRec struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	APIKey   string `json:"api_key"`
	Conf     string `json:"conf"`
}

type systemRec struct {
	Text    *string `json:"text,omitempty"`
	Static  *string `json:"static,omitempty"`
	Dynamic *string `json:"dynamic,omitempty"`
}

type callRec struct {
	System systemRec `json:"system"`
	User   string    `json:"user"`
	Opts   string    `json:"opts"`
}

type answer struct {
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
	Status *int            `json:"status"`
}

type scenario struct {
	Name   string                     `json:"name"`
	Config map[string]string          `json:"config"`
	Unset  []string                   `json:"unset"`
	Rows   map[string]json.RawMessage `json:"rows"`
	Fail   map[string]string          `json:"fail"`
	// FailNth fails the nth call of a kind with its message; null passes.
	FailNth     map[string][]*string `json:"failNth"`
	NotesIn     map[string]string    `json:"notesIn"`
	Search      json.RawMessage      `json:"search"`
	SearchError string               `json:"searchError"`
	Answers     []answer             `json:"answers"`
	Clock       float64              `json:"clock"`
	Outcome     outcome              `json:"outcome"`
	Queries     []queryRec           `json:"queries"`
	Logs        []logRec             `json:"logs"`
	Events      []eventRec           `json:"events"`
	Notes       []noteRec            `json:"notes"`
	Created     []createdRec         `json:"created"`
	Calls       []callRec            `json:"calls"`
	Timers      []float64            `json:"timers"`
	Reads       []readRec            `json:"reads"`
	Searches    []searchRec          `json:"searches"`
	Errors      []logRec             `json:"errors"`
}

type goldenFile struct {
	ProviderConf []struct {
		Row json.RawMessage `json:"row"`
		Out outcome         `json:"out"`
	} `json:"provider_conf"`
	Cadence []struct {
		Value    string    `json:"value"`
		Fallback string    `json:"fallback"`
		Out      []float64 `json:"out"`
		Total1   float64   `json:"total1"`
		Total5   float64   `json:"total5"`
	} `json:"cadence"`
	Prepare []struct {
		In  string  `json:"in"`
		Out *string `json:"out"`
	} `json:"prepare"`
	Extraction []struct {
		Type string `json:"type"`
		Hint string `json:"hint"`
		Out  string `json:"out"`
	} `json:"extraction"`
	Limits []struct {
		Row     json.RawMessage   `json:"row"`
		Config  map[string]string `json:"config"`
		Daily   string            `json:"daily"`
		Monthly string            `json:"monthly"`
	} `json:"limits"`
	Limiter []struct {
		scenario
		Ops     [][]any         `json:"ops"`
		Results json.RawMessage `json:"results"`
	} `json:"limiter"`
	Invoke []struct {
		scenario
		Agent string          `json:"agent"`
		Opts  json.RawMessage `json:"opts"`
		Twice bool            `json:"twice"`
	} `json:"invoke"`
	CallLog []struct {
		scenario
		Call json.RawMessage `json:"call"`
	} `json:"call_log"`
	Transcript []struct {
		scenario
		Args json.RawMessage `json:"args"`
	} `json:"transcript"`
	Learning []struct {
		scenario
		Args json.RawMessage `json:"args"`
	} `json:"learning"`
	Retry []struct {
		scenario
		Results        []string `json:"results"`
		CallbackThrows bool     `json:"callback_throws"`
		FirstFailure   []struct {
			Error string `json:"error"`
			Info  string `json:"info"`
		} `json:"first_failure"`
	} `json:"retry"`
	Activity []struct {
		scenario
		Agent   string  `json:"agent"`
		Advance float64 `json:"advance"`
		Throws  string  `json:"throws"`
	} `json:"activity"`
	Ping []scenario `json:"ping"`
}

func loadGoldens(t *testing.T) *goldenFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/goldens.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldenFile
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return &g
}

var baseConfig = map[string]string{
	"virtual_agent_rate_limit":                 "10",
	"virtual_agent_rate_window_seconds":        "60",
	"virtual_agent_cooldown_seconds":           "300",
	"virtual_agent_default_daily_budget":       "1.00",
	"virtual_agent_default_monthly_budget":     "10.00",
	"virtual_agent_retry_backoff":              "300,600,3600",
	"virtual_agent_retry_backoff_client_error": "5,15,60",
	"virtual_agent_learning_enabled":           "true",
	"virtual_agent_learning_min_tokens":        "20",
	"virtual_agent_error_ping_interval":        "15",
	"global_bootstrap":                         "",
	"people_context_preamble":                  "Your impressions follow.",
}

type mapConfig map[string]string

func (m mapConfig) Get(key string) (string, error) {
	v, ok := m[key]
	if !ok {
		return "", fmt.Errorf("Config key \"%s\" not found. Was config.init() called?", key)
	}
	return v, nil
}

// recorder is the scenario being replayed: the stubs record into it and
// answer from the golden's rows and answers.
type recorder struct {
	t      *testing.T
	sc     *scenario
	mu     sync.Mutex
	clock  float64
	counts map[string]int
	nth    map[string]int
	// sql is the text each query kind was last sent with
	// (chat_golden_test.go compares the direct-chat kinds with Node's).
	sql map[string]string
	// slept holds the contexts Sleep was called with.
	slept  map[context.Context]bool
	nCalls int

	// block makes a query kind wait for its context to end; onCall runs
	// inside each provider call.
	block  map[string]bool
	onCall func()

	queries  []queryRec
	logs     []logRec
	events   []eventRec
	notes    []noteRec
	created  []createdRec
	calls    []callRec
	timers   []float64
	reads    []readRec
	searches []searchRec
	errors   []logRec
}

var kinds = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"load-agent", regexp.MustCompile(`SELECT ac\.id AS actor_id, ac\.name AS agent`)},
	{"rate-overrides", regexp.MustCompile(`SELECT ac\.configuration FROM agent_configuration`)},
	{"cost-sums", regexp.MustCompile(`SUM\(CASE WHEN created_at`)},
	{"usage-insert", regexp.MustCompile(`INSERT INTO virtual_agent_usage`)},
	{"call-insert", regexp.MustCompile(`INSERT INTO virtual_agent_calls`)},
	{"active-on", regexp.MustCompile(`UPDATE actors SET active_since = NOW\(\)`)},
	{"active-off", regexp.MustCompile(`UPDATE actors SET active_since = NULL`)},
	{"status", regexp.MustCompile(`UPDATE actors SET status`)},
	{"dream-mode", regexp.MustCompile(`SELECT agc\.dream_mode`)},
	{"errored-scan", regexp.MustCompile(`WHERE agc\.virtual = TRUE AND ac\.status`)},
	{"mail-load", regexp.MustCompile(`JOIN actors fa ON fa\.id = m\.from_actor_id`)},
	{"mail-ack", regexp.MustCompile(`UPDATE mail SET acked_at = NOW\(\)`)},
	// Direct chat (chat_golden_test.go).
	{"chat-history", regexp.MustCompile(`JOIN actors fa ON fa\.id = cmt\.from_actor_id\s+JOIN actors ta`)},
	{"chat-ack", regexp.MustCompile(`UPDATE chat_messages SET acked_at = NOW\(\) WHERE id = \$1 AND to_actor_id`)},
}

// paramText is a Go query argument as the text Postgres receives, to
// compare with node-postgres's prepared value.
func paramText(t *testing.T, v any) *string {
	s := func(x string) *string { return &x }
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return s(x)
	case jsutil.Number:
		return s(string(x))
	case int:
		return s(strconv.Itoa(x))
	case int64:
		return s(strconv.FormatInt(x, 10))
	case bool:
		return s(strconv.FormatBool(x))
	}
	t.Errorf("unexpected query argument %T %v", v, v)
	return s(fmt.Sprint(v))
}

func (r *recorder) query(ctx context.Context, sql string, args []any) (string, []any, error) {
	kind := ""
	for _, k := range kinds {
		if k.re.MatchString(sql) {
			kind = k.kind
			break
		}
	}
	if kind == "" {
		r.t.Fatalf("unclassified query: %s", sql)
	}
	if r.block[kind] {
		<-ctx.Done()
	}
	if err := ctx.Err(); err != nil {
		return kind, nil, err
	}
	params := make([]*string, len(args))
	for i, a := range args {
		params[i] = paramText(r.t, a)
	}
	// The spinner's off-switch runs on the context it slept on; Node's ran
	// inside its setTimeout callback.
	r.mu.Lock()
	timer := kind == "active-off" && r.slept[ctx]
	if r.sql == nil {
		r.sql = map[string]string{}
	}
	r.sql[kind] = sql
	r.mu.Unlock()
	rows, err := r.record(kind, params, timer)
	return kind, rows, err
}

// record logs one query (or one mail send, kind "mail-send"), fails it as
// the scenario says, and answers from the scenario's rows.
func (r *recorder) record(kind string, params []*string, timer bool) ([]any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, queryRec{kind, params, timer})
	r.nth[kind]++
	if nth := r.sc.FailNth[kind]; r.nth[kind] <= len(nth) && nth[r.nth[kind]-1] != nil {
		return nil, errors.New(*nth[r.nth[kind]-1])
	}
	if msg := r.sc.Fail[kind]; msg != "" {
		return nil, errors.New(msg)
	}
	raw := r.sc.Rows[kind]
	if len(raw) == 0 {
		return nil, nil
	}
	v, err := jsutil.Parse(raw)
	if err != nil {
		r.t.Fatal(err)
	}
	rows, _ := v.([]any)
	if len(rows) > 0 {
		if _, queue := rows[0].([]any); queue {
			r.counts[kind]++
			n := min(r.counts[kind], len(rows))
			rows, _ = rows[n-1].([]any)
		}
	}
	return rows, nil
}

// assign scans a golden row value into a destination.
func assign(dest, v any) error {
	switch d := dest.(type) {
	case *int:
		f, _ := jsutil.AsFloat(v)
		*d = int(f)
	case **int:
		if v == nil {
			*d = nil
			return nil
		}
		f, _ := jsutil.AsFloat(v)
		n := int(f)
		*d = &n
	case *string:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("want string, got %T", v)
		}
		*d = s
	case **string:
		if v == nil {
			*d = nil
			return nil
		}
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("want string, got %T", v)
		}
		*d = &s
	case *bool:
		b, _ := v.(bool)
		*d = b
	case *[]byte:
		*d = []byte(jsutil.Marshal(v))
	case **time.Time:
		if v == nil {
			*d = nil
			return nil
		}
		s, _ := v.(string)
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return err
		}
		*d = &ts
	default:
		return fmt.Errorf("unsupported scan target %T", dest)
	}
	return nil
}

func scanRow(row any, dest []any) error {
	o, ok := row.(*jsutil.Object)
	if !ok {
		return fmt.Errorf("row is %T", row)
	}
	keys := o.Keys()
	if len(keys) != len(dest) {
		return fmt.Errorf("row has %d columns, scan has %d", len(keys), len(dest))
	}
	for i, k := range keys {
		if err := assign(dest[i], o.Value(k)); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

type fakeDB struct{ r *recorder }

func (f fakeDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	_, _, err := f.r.query(ctx, sql, args)
	return pgconn.CommandTag{}, err
}

type fakeRow struct {
	rows []any
	err  error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(r.rows) == 0 {
		return pgx.ErrNoRows
	}
	return scanRow(r.rows[0], dest)
}

func (f fakeDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	_, rows, err := f.r.query(ctx, sql, args)
	return fakeRow{rows, err}
}

func (f fakeDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	_, rows, err := f.r.query(ctx, sql, args)
	if err != nil {
		return nil, err
	}
	return &fakeRows{rows: rows, i: -1}, nil
}

func (f fakeDB) Begin(ctx context.Context) (pgx.Tx, error) {
	return nil, errors.New("no transactions here")
}

type fakeRows struct {
	rows []any
	i    int
	err  error
}

func (r *fakeRows) Close()                                       {}
func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Next() bool                                   { r.i++; return r.i < len(r.rows) }
func (r *fakeRows) Scan(dest ...any) error                       { return scanRow(r.rows[r.i], dest) }
func (r *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }
func (r *fakeRows) TypeMap() *pgtype.Map                         { return nil }

func (r *recorder) Log(subsystem, action string, details logger.Fields) {
	b, err := jsonutil.Marshal(details)
	if err != nil {
		r.t.Fatal(err)
	}
	r.mu.Lock()
	r.logs = append(r.logs, logRec{subsystem, action, strings.TrimSuffix(string(b), "\n")})
	r.mu.Unlock()
}

func (r *recorder) Broadcast(event string, data any) {
	b, err := jsonutil.Marshal(data)
	if err != nil {
		r.t.Fatal(err)
	}
	r.mu.Lock()
	r.events = append(r.events, eventRec{event, strings.TrimSuffix(string(b), "\n")})
	r.mu.Unlock()
}

var actorIDs = map[string]int{"va": 7, "other": 8, "router": 9}

func (r *recorder) RequireByName(ctx context.Context, name string) (actors.Actor, error) {
	id, ok := actorIDs[name]
	if !ok {
		return actors.Actor{}, apierr.New(404, fmt.Sprintf("Agent \"%s\" is not registered", name))
	}
	return actors.Actor{ID: id, Name: name}, nil
}

var slugSuffix = regexp.MustCompile(`-[0-9a-z]{0,4}$`)

func (r *recorder) Save(ctx context.Context, in notes.SaveInput) (notes.SavedNote, error) {
	content, _ := json.Marshal(in.Content)
	if jsutil.UTF16Len(in.Content) > 2000 {
		content, _ = json.Marshal(struct {
			Length int    `json:"length"`
			Head   string `json:"head"`
			Tail   string `json:"tail"`
		}{jsutil.UTF16Len(in.Content), jsutil.TruncateUTF16(in.Content, 300), in.Content[len(in.Content)-300:]})
	}
	r.mu.Lock()
	r.notes = append(r.notes, noteRec{in.Namespace, in.Title, content, slugSuffix.ReplaceAllString(in.Slug, "-XXXX"), in.CreatedBy})
	r.mu.Unlock()
	return notes.SavedNote{ID: 1}, nil
}

func (r *recorder) DecryptAPIKey(k string) (string, error) {
	return "dec:" + k, nil
}

func (r *recorder) CreateProvider(provider, model, apiKey string, conf any) (providers.Call, error) {
	r.mu.Lock()
	r.created = append(r.created, createdRec{provider, model, apiKey, jsutil.Marshal(conf)})
	r.mu.Unlock()
	return func(ctx context.Context, system providers.SystemPrompt, user string, opts *providers.RawOpts) (*providers.Result, error) {
		var sys systemRec
		if system.Static == "" && system.Dynamic == "" {
			text := providers.FlattenPrompt(system)
			sys.Text = &text
		} else {
			sys.Static, sys.Dynamic = &system.Static, &system.Dynamic
		}
		// No options is Node's undefined, which JSON.stringify leaves out.
		optsText := ""
		if opts != nil {
			o := jsutil.NewObject()
			if opts.Cache != nil {
				o.Set("cache", opts.Cache)
			}
			if opts.Tools != nil {
				o.Set("tools", opts.Tools)
			}
			if opts.Messages != nil {
				o.Set("messages", opts.Messages)
			}
			if opts.TurnContext != nil {
				o.Set("turnContext", opts.TurnContext)
			}
			optsText = jsutil.Marshal(o)
		}
		r.mu.Lock()
		r.calls = append(r.calls, callRec{sys, user, optsText})
		n := r.nCalls
		r.nCalls++
		onCall := r.onCall
		r.mu.Unlock()
		if onCall != nil {
			onCall()
		}
		a := r.sc.Answers[min(n, len(r.sc.Answers)-1)]
		if a.Error != "" {
			if a.Status != nil {
				return nil, &providers.APIError{Status: *a.Status, Message: a.Error}
			}
			return nil, errors.New(a.Error)
		}
		return resultFrom(r.t, a.Result), nil
	}, nil
}

func resultFrom(t *testing.T, raw json.RawMessage) *providers.Result {
	v, err := jsutil.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	o := v.(*jsutil.Object)
	res := &providers.Result{Text: o.Value("text")}
	if calls, ok := o.Value("tool_calls").([]any); ok {
		res.ToolCalls = []providers.ToolCall{}
		for _, c := range calls {
			co := c.(*jsutil.Object)
			res.ToolCalls = append(res.ToolCalls, providers.ToolCall{ID: co.Value("id"), Name: co.Value("name"), Input: co.Value("input")})
		}
	}
	if u, ok := o.Value("usage").(*jsutil.Object); ok {
		get := func(k string) any {
			if v, ok := u.Get(k); ok {
				return v
			}
			return nil
		}
		res.Usage = providers.Usage{
			InputTokens: get("input_tokens"), OutputTokens: get("output_tokens"),
			CacheCreationInputTokens: get("cache_creation_input_tokens"), CacheReadInputTokens: get("cache_read_input_tokens"),
		}
		if c, ok := jsutil.AsFloat(get("cost")); ok {
			res.Usage.Cost = &c
		}
		if s, ok := get("served_by").(string); ok {
			res.Usage.ServedBy = &s
		}
	}
	if f, ok := o.Value("finish_reason").(string); ok {
		res.FinishReason = f
	}
	res.Truncated = o.Value("truncated") == true
	return res
}

// newService builds the service over a scenario's stubs.
func newService(t *testing.T, sc *scenario) (*Service, *recorder) {
	t.Helper()
	r := &recorder{t: t, sc: sc, counts: map[string]int{}, nth: map[string]int{}, slept: map[context.Context]bool{}, clock: sc.Clock}
	if r.clock == 0 {
		r.clock = 1793990000000
	}
	cfg := mapConfig{}
	for k, v := range baseConfig {
		cfg[k] = v
	}
	for k, v := range sc.Config {
		cfg[k] = v
	}
	for _, k := range sc.Unset {
		delete(cfg, k)
	}
	s := &Service{
		DB: fakeDB{r}, Config: cfg, Actors: r, Log: r, Events: r, Providers: r, Notes: r, Memory: r, Mail: r,
		Now: func() time.Time {
			r.mu.Lock()
			defer r.mu.Unlock()
			return time.UnixMilli(int64(r.clock))
		},
		Sleep: func(ctx context.Context, ms float64) error {
			r.mu.Lock()
			r.slept[ctx] = true
			r.timers = append(r.timers, ms)
			r.mu.Unlock()
			return nil
		},
	}
	return s, r
}

func (r *recorder) advance(ms float64) {
	r.mu.Lock()
	r.clock += ms
	r.mu.Unlock()
}

func errText(err error) outcome {
	msg := err.Error()
	return outcome{Error: &msg}
}

func okJSON(v any) outcome {
	var s string
	switch x := v.(type) {
	case json.Marshaler:
		b, _ := x.MarshalJSON()
		s = string(b)
	default:
		s = jsutil.Marshal(v)
	}
	return outcome{OK: &s}
}

// background reorders what Go writes in the background (the call log and
// its failure line) to the end, as their place against the synchronous
// writes is not fixed.
func background[T any](items []T, isBackground func(T) bool) []T {
	var front, back []T
	for _, it := range items {
		if isBackground(it) {
			back = append(back, it)
		} else {
			front = append(front, it)
		}
	}
	return append(front, back...)
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// compare waits for the background writes, then compares every recording.
func compare(t *testing.T, want *scenario, r *recorder, got outcome) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		done := len(r.queries) >= len(want.Queries) && len(r.logs) >= len(want.Logs) && len(r.events) >= len(want.Events)
		r.mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(10 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()

	if jsonOf(got) != jsonOf(want.Outcome) {
		t.Errorf("outcome:\n got %s\nwant %s", jsonOf(got), jsonOf(want.Outcome))
	}
	isCall := func(q queryRec) bool { return q.Kind == "call-insert" }
	if g, w := jsonOf(background(r.queries, isCall)), jsonOf(background(want.Queries, isCall)); g != w {
		t.Errorf("queries:\n got %s\nwant %s", g, w)
	}
	isCallLog := func(l logRec) bool { return l.Action == "call-log-error" }
	if g, w := jsonOf(background(r.logs, isCallLog)), jsonOf(background(want.Logs, isCallLog)); g != w {
		t.Errorf("logs:\n got %s\nwant %s", g, w)
	}
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"events", r.events, want.Events},
		{"notes", r.notes, want.Notes},
		{"created", r.created, want.Created},
		{"calls", r.calls, want.Calls},
		{"timers", r.timers, want.Timers},
	} {
		g, w := jsonOf(c.got), jsonOf(c.want)
		if g == "null" {
			g = "[]"
		}
		if w == "null" {
			w = "[]"
		}
		if g != w {
			t.Errorf("%s:\n got %s\nwant %s", c.name, g, w)
		}
	}
}

// agentFrom builds an Agent from a golden agent row.
func agentFrom(t *testing.T, raw json.RawMessage) *Agent {
	t.Helper()
	v, err := jsutil.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var a Agent
	dest := []any{&a.ActorID, &a.Name, &a.Virtual, &a.Provider, &a.Model, &a.APIKey, &a.Configuration,
		&a.StartupInstructions, &a.Personality, &a.Expertise, &a.CostBudgetDaily, &a.CostBudgetMonthly,
		&a.CachePrompts, &a.LearningEnabled, &a.MaxTokens, &a.Temperature}
	if err := scanRow(v, dest); err != nil {
		t.Fatal(err)
	}
	return &a
}

func systemFrom(v any) providers.SystemPrompt {
	if o, ok := v.(*jsutil.Object); ok {
		st, _ := o.Value("static").(string)
		dy, _ := o.Value("dynamic").(string)
		return providers.StructuredPrompt(st, dy)
	}
	s, _ := v.(string)
	return providers.TextPrompt(s)
}

func TestProviderConfMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).ProviderConf {
		a := agentFrom(t, c.Row)
		var got outcome
		conf, err := BuildProviderConf(a)
		if err != nil {
			got = errText(err)
		} else {
			s := jsutil.Marshal(conf)
			got = outcome{OK: &s}
		}
		if jsonOf(got) != jsonOf(c.Out) {
			t.Errorf("%s:\n got %s\nwant %s", c.Row, jsonOf(got), jsonOf(c.Out))
		}
	}
}

func TestCadenceMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Cadence {
		got := parseBackoffCadence(c.Value, c.Fallback)
		if jsonOf(got) != jsonOf(c.Out) {
			t.Errorf("%q: got %v want %v", c.Value, got, c.Out)
		}
		if a, b := totalDelayForCadence(got, 1), totalDelayForCadence(got, 5); a != c.Total1 || b != c.Total5 {
			t.Errorf("%q totals: got %v %v want %v %v", c.Value, a, b, c.Total1, c.Total5)
		}
	}
}

func TestExtractionPromptMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Extraction {
		if got := buildExtractionPrompt(c.Type, c.Hint); got != c.Out {
			t.Errorf("%s: got %q want %q", c.Type, got, c.Out)
		}
	}
}

func TestEffectiveLimitsMatchNode(t *testing.T) {
	for _, c := range loadGoldens(t).Limits {
		cfg := mapConfig{}
		for k, v := range baseConfig {
			cfg[k] = v
		}
		for k, v := range c.Config {
			cfg[k] = v
		}
		var row struct {
			Daily   *string `json:"cost_budget_daily"`
			Monthly *string `json:"cost_budget_monthly"`
		}
		json.Unmarshal(c.Row, &row)
		s := &Service{Config: cfg}
		l, err := s.ResolveEffectiveLimits(&Agent{CostBudgetDaily: row.Daily, CostBudgetMonthly: row.Monthly})
		if err != nil {
			t.Fatal(err)
		}
		text := func(f *float64) string {
			if f == nil {
				return "null"
			}
			return jsutil.NumberString(*f)
		}
		if text(l.Daily) != c.Daily || text(l.Monthly) != c.Monthly {
			t.Errorf("%s %v: got %s %s want %s %s", c.Row, c.Config, text(l.Daily), text(l.Monthly), c.Daily, c.Monthly)
		}
	}
}

func TestLimiterMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Limiter {
		t.Run(c.Name, func(t *testing.T) {
			s, r := newService(t, &c.scenario)
			if c.Name == "no window setting" {
				delete(s.Config.(mapConfig), "virtual_agent_rate_window_seconds")
			}
			ctx := context.Background()
			var results []any
			for _, op := range c.Ops {
				name := op[0].(string)
				scene := ""
				if len(op) > 1 {
					if s, ok := op[1].(string); ok {
						scene = s
					}
				}
				switch name {
				case "limited":
					limited, err := s.IsRateLimited(ctx, "va", scene)
					if err != nil {
						results = append(results, "error: "+err.Error())
					} else {
						results = append(results, limited)
					}
				case "record":
					if err := s.RecordCall("va", scene); err != nil {
						results = append(results, "error: "+err.Error())
					} else {
						results = append(results, "recorded")
					}
				case "resume":
					results = append(results, s.RateLimitResumeSeconds("va"))
				case "advance":
					r.advance(op[1].(float64))
					results = append(results, "advanced")
				case "effective":
					rl, err := s.EffectiveRateLimit(ctx, "va")
					if err != nil {
						results = append(results, "error: "+err.Error())
						break
					}
					o := jsutil.NewObject()
					o.Set("limit", jsNumber(rl.Limit))
					o.Set("windowMs", jsNumber(rl.WindowMs))
					o.Set("cooldownMs", jsNumber(rl.CooldownMs))
					results = append(results, jsutil.Marshal(o))
				}
			}
			want, _ := jsutil.Parse(c.Results)
			if g, w := jsutil.Marshal(results), jsutil.Marshal(want); g != w {
				t.Errorf("results:\n got %s\nwant %s", g, w)
			}
			compare(t, &c.scenario, r, outcome{})
		})
	}
}

func TestInvokeMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Invoke {
		t.Run(c.Name, func(t *testing.T) {
			s, r := newService(t, &c.scenario)
			v, _ := jsutil.Parse(c.Opts)
			o := v.(*jsutil.Object)
			var opts InvokeOptions
			if sp, ok := o.Value("systemPrompt").(string); ok {
				p := providers.TextPrompt(sp)
				opts.SystemPrompt = &p
			}
			opts.UserMessage, _ = o.Value("userMessage").(string)
			opts.Context, _ = o.Value("context").(string)
			if m, ok := o.Get("messages"); ok {
				opts.Messages = m
			}
			if tl, ok := o.Get("tools"); ok {
				opts.Tools = tl
			}
			opts.SkipRateLimit = o.Value("skipRateLimit") == true
			opts.SkipCostLimit = o.Value("skipCostLimit") == true
			opts.Retry = o.Value("skipRetry") == false
			ctx := context.Background()
			if c.Twice {
				if _, err := s.Invoke(ctx, c.Agent, opts); err != nil {
					t.Fatalf("first call: %v", err)
				}
			}
			res, err := s.Invoke(ctx, c.Agent, opts)
			var got outcome
			if err != nil {
				got = errText(err)
			} else {
				got = okJSON(res)
			}
			compare(t, &c.scenario, r, got)
		})
	}
}

func TestCallLogMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).CallLog {
		t.Run(c.Name, func(t *testing.T) {
			s, r := newService(t, &c.scenario)
			v, _ := jsutil.Parse(c.Call)
			o := v.(*jsutil.Object)
			str := func(k string) string { x, _ := o.Value(k).(string); return x }
			num := func(k string) float64 { f, _ := jsutil.AsFloat(o.Value(k)); return f }
			cl := CallLog{
				ActorID: int(num("actorId")), AgentName: str("agentName"), Context: str("context"), ContextID: str("contextId"),
				Provider: str("provider"), Model: str("model"), UserMessage: str("userMessage"), Response: o.Value("response"),
				Cost: num("cost"), Duration: time.Duration(num("durationMs")) * time.Millisecond,
				SceneID: str("sceneId"), ConversationID: str("conversationId"), SimActorID: str("simActorId"), SimActorName: str("simActorName"),
			}
			if sp, ok := o.Get("systemPrompt"); ok {
				p := systemFrom(sp)
				cl.SystemPrompt = &p
			}
			if id := int(num("usageId")); id != 0 {
				cl.UsageID = &id
			}
			if u, ok := o.Value("usage").(*jsutil.Object); ok {
				usage := resultFrom(t, json.RawMessage(`{"usage":`+jsutil.Marshal(u)+`}`)).Usage
				cl.Usage = &usage
			}
			if e, ok := o.Value("error").(*jsutil.Object); ok {
				msg, _ := e.Value("message").(string)
				if st, ok := jsutil.AsFloat(e.Value("status")); ok {
					cl.Err = &providers.APIError{Status: int(st), Message: msg}
				} else if sc, ok := jsutil.AsFloat(e.Value("statusCode")); ok {
					cl.Err = apierr.New(int(sc), msg)
				} else {
					cl.Err = errors.New(msg)
				}
			}
			s.LogCall(context.Background(), cl)
			compare(t, &c.scenario, r, outcome{})
		})
	}
}

func TestTranscriptMatchesNode(t *testing.T) {
	old := localZone
	localZone = time.FixedZone("IST", 5*3600+1800)
	defer func() { localZone = old }()
	for _, c := range loadGoldens(t).Transcript {
		t.Run(c.Name, func(t *testing.T) {
			s, r := newService(t, &c.scenario)
			var args []any
			switch c.Name {
			case "truncate system":
				args = []any{"va", strings.Repeat("S", 5000), strings.Repeat("u", 97000), "R", nil, "chat", jsutil.NewObject()}
			case "truncate both":
				args = []any{"va", strings.Repeat("S", 5000), strings.Repeat("x", 99500) + strings.Repeat("y", 1000), "R", nil, "chat", jsutil.NewObject()}
			default:
				v, _ := jsutil.Parse(c.Args)
				args = v.([]any)
			}
			var usage *providers.Usage
			if u, ok := args[4].(*jsutil.Object); ok {
				x := resultFrom(t, json.RawMessage(`{"usage":`+jsutil.Marshal(u)+`}`)).Usage
				usage = &x
			}
			meta := TranscriptMeta{}
			if m, ok := args[6].(*jsutil.Object); ok {
				meta.RequestingAgent, _ = m.Value("requestingAgent").(string)
				meta.Model, _ = m.Value("model").(string)
				if d, ok := jsutil.AsFloat(m.Value("discussionId")); ok {
					meta.DiscussionID = int(d)
				}
			}
			user, _ := args[2].(string)
			err := s.LogTranscript(context.Background(), args[0].(string), systemFrom(args[1]), user, args[3], usage, args[5].(string), meta)
			got := outcome{}
			if err != nil {
				got = errText(err)
			}
			compare(t, &c.scenario, r, got)
		})
	}
}

func TestLearningMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Learning {
		t.Run(c.Name, func(t *testing.T) {
			s, r := newService(t, &c.scenario)
			var args []json.RawMessage
			json.Unmarshal(c.Args, &args)
			a := agentFrom(t, args[0])
			sys, _ := jsutil.Parse(args[1])
			resp, _ := jsutil.Parse(args[3])
			var user, typ, hint string
			json.Unmarshal(args[2], &user)
			json.Unmarshal(args[4], &typ)
			json.Unmarshal(args[5], &hint)
			err := s.ExtractLearnings(context.Background(), a, systemFrom(sys), user, resp, typ, hint)
			got := outcome{}
			if err != nil {
				got = errText(err)
			}
			compare(t, &c.scenario, r, got)
		})
	}
}

func TestRetryMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Retry {
		t.Run(c.Name, func(t *testing.T) {
			s, r := newService(t, &c.scenario)
			n := 0
			var seen []map[string]string
			res, err := retryWithBackoff(context.Background(), s, "va", func(ctx context.Context) (string, error) {
				step := c.Results[n]
				n++
				if step == "ok" {
					return "done", nil
				}
				msg := fmt.Sprintf("attempt %d", n)
				if _, status, ok := strings.Cut(step, ":"); ok {
					code, _ := strconv.Atoi(status)
					return "", &providers.APIError{Status: code, Message: msg}
				}
				return "", errors.New(msg)
			}, func(err error, info RetryInfo) error {
				o := jsutil.NewObject()
				o.Set("retriesRemaining", info.RetriesRemaining)
				o.Set("totalSeconds", info.TotalSeconds)
				seen = append(seen, map[string]string{"error": err.Error(), "info": jsutil.Marshal(o)})
				if c.CallbackThrows {
					return errors.New("callback broke")
				}
				return nil
			})
			got := okJSON(res)
			if err != nil {
				got = errText(err)
			}
			if jsonOf(seen) != jsonOf(c.FirstFailure) && !(len(seen) == 0 && len(c.FirstFailure) == 0) {
				t.Errorf("first failure:\n got %s\nwant %s", jsonOf(seen), jsonOf(c.FirstFailure))
			}
			compare(t, &c.scenario, r, got)
		})
	}
}

func TestActivityMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Activity {
		t.Run(c.Name, func(t *testing.T) {
			s, r := newService(t, &c.scenario)
			res, err := WithActivity(context.Background(), s, c.Agent, func() (string, error) {
				r.advance(c.Advance)
				if c.Throws != "" {
					return "", errors.New(c.Throws)
				}
				return "v", nil
			})
			got := okJSON(res)
			if err != nil {
				got = errText(err)
			}
			compare(t, &c.scenario, r, got)
		})
	}
}

func TestPingMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Ping {
		t.Run(c.Name, func(t *testing.T) {
			s, r := newService(t, &c)
			s.pingErroredAgents(context.Background())
			compare(t, &c, r, outcome{})
		})
	}
}

func TestPrepareValueMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Prepare {
		v, err := jsutil.Parse([]byte(c.In))
		if err != nil {
			t.Fatal(err)
		}
		var got *string
		if text, ok := prepareValue(v); ok {
			got = &text
		}
		if jsonOf(got) != jsonOf(c.Out) {
			t.Errorf("%s: got %s want %s", c.In, jsonOf(got), jsonOf(c.Out))
		}
	}
}

// bookkeepingScenario is an Invoke whose agent loads and whose limits pass.
func bookkeepingScenario(answers string) *scenario {
	sc := &scenario{Rows: map[string]json.RawMessage{
		"load-agent":   json.RawMessage(`[{"actor_id":7,"agent":"va","virtual":true,"provider":"anthropic","model":"claude-haiku-4-5","api_key":"enc","configuration":"{\"_configVersion\":1}","startup_instructions":"S","personality":null,"expertise":[],"cost_budget_daily":null,"cost_budget_monthly":null,"cache_prompts":false,"learning_enabled":true,"max_tokens":null,"temperature":null}]`),
		"cost-sums":    json.RawMessage(`[{"cost_today":"0","cost_monthly":"0"}]`),
		"usage-insert": json.RawMessage(`[{"id":5}]`),
	}}
	if err := json.Unmarshal([]byte(answers), &sc.Answers); err != nil {
		panic(err)
	}
	return sc
}

func (r *recorder) kinds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, q := range r.queries {
		out = append(out, q.Kind)
	}
	return out
}

// A caller that hangs up during the provider call does not lose the usage
// row, the active-state reset or the call log.
func TestInvokeBookkeepingSurvivesCancel(t *testing.T) {
	sc := bookkeepingScenario(`[{"result":{"text":"ok","tool_calls":[],"usage":{"input_tokens":1,"output_tokens":1},"finish_reason":"end_turn","truncated":false}}]`)
	s, r := newService(t, sc)
	ctx, cancel := context.WithCancel(context.Background())
	r.onCall = cancel
	if _, err := s.Invoke(ctx, "va", InvokeOptions{UserMessage: "Hi", Context: "tick"}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(strings.Join(r.kinds(), ","), "call-insert") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := strings.Join(r.kinds(), ",")
	for _, k := range []string{"active-off", "usage-insert", "call-insert"} {
		if !strings.Contains(got, k) {
			t.Errorf("%s missing after the caller hung up: %s", k, got)
		}
	}
}

// A stuck usage write gives up after bookkeepingTimeout, and the
// active-state reset still runs with its own bound.
func TestInvokeBookkeepingIsBounded(t *testing.T) {
	old := bookkeepingTimeout
	bookkeepingTimeout = 50 * time.Millisecond
	defer func() { bookkeepingTimeout = old }()
	sc := bookkeepingScenario(`[{"error":"provider down","status":503}]`)
	s, r := newService(t, sc)
	r.block = map[string]bool{"usage-insert": true}
	done := make(chan error, 1)
	go func() {
		_, err := s.Invoke(context.Background(), "va", InvokeOptions{UserMessage: "Hi"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || err.Error() != "provider down" {
			t.Fatalf("Invoke error = %v, want the provider's", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Invoke did not return while the usage write was stuck")
	}
	got := strings.Join(r.kinds(), ",")
	if !strings.Contains(got, "active-off") {
		t.Errorf("active-off missing after a stuck usage write: %s", got)
	}
	if strings.Contains(got, "usage-insert") {
		t.Errorf("the stuck usage write should not have completed: %s", got)
	}
}

// The tick route tells the two refusals apart by type.
func TestInvokeRefusalsAreTyped(t *testing.T) {
	sc := bookkeepingScenario(`[{"result":{"text":"ok","tool_calls":[],"usage":{"input_tokens":1,"output_tokens":1},"finish_reason":"end_turn","truncated":false}}]`)
	sc.Config = map[string]string{"virtual_agent_rate_limit": "0"}
	s, _ := newService(t, sc)
	_, err := s.Invoke(context.Background(), "va", InvokeOptions{UserMessage: "Hi"})
	var rl *RateLimitedError
	if !errors.As(err, &rl) || err.Error() != "Agent va is rate-limited" {
		t.Errorf("rate limit: got %v", err)
	}
	sc = bookkeepingScenario(`[]`)
	sc.Rows["cost-sums"] = json.RawMessage(`[{"cost_today":"5","cost_monthly":"5"}]`)
	s, _ = newService(t, sc)
	_, err = s.Invoke(context.Background(), "va", InvokeOptions{UserMessage: "Hi"})
	var oc *OverCostLimitError
	if !errors.As(err, &oc) || err.Error() != "Agent va over cost limit: Daily cost limit exceeded ($5.0000 / $1.00)" {
		t.Errorf("cost limit: got %v", err)
	}
}
