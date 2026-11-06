package virtualagent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/chat"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
)

// The direct-chat goldens (testdata/goldens_chat.js) run Node's
// handleDirectChat and the functions only it uses with the same stubs as
// goldens.js, plus recorders for chatSend, the memory search, note reads
// and logError.

func init() {
	for name, id := range map[string]int{"salem-engine": 10, "salem-magistrate": 11, "salem-visitor": 12, "human": 13} {
		actorIDs[name] = id
	}
}

type chatInput struct {
	Agent      string  `json:"agent"`
	From       string  `json:"from"`
	Message    *string `json:"message"`
	MessageIDs []int   `json:"messageIds"`
	Opts       string  `json:"opts"`
}

type chatScenario struct {
	scenario
	// Unordered compares every recording as a multiset: the learning
	// extraction runs in parallel with the transcript.
	Unordered bool              `json:"unordered"`
	Outcomes  []json.RawMessage `json:"outcomes"`
	Inputs    []chatInput       `json:"inputs"`
}

type chatGoldens struct {
	Wrap []struct {
		Args []*string `json:"args"`
		Out  string    `json:"out"`
	} `json:"wrap"`
	Paraphrase []struct {
		Name          json.RawMessage `json:"name"`
		NameUndefined bool            `json:"name_undefined"`
		Input         json.RawMessage `json:"input"`
		Out           outcome         `json:"out"`
	} `json:"paraphrase"`
	Gap []struct {
		Ms  *float64 `json:"ms"`
		Out *string  `json:"out"`
	} `json:"gap"`
	CoLocated []struct {
		Text string   `json:"text"`
		Out  []string `json:"out"`
	} `json:"colocated"`
	Strip []struct {
		Text string `json:"text"`
		Out  string `json:"out"`
	} `json:"strip"`
	Speakers []struct {
		Names []*string `json:"names"`
		Self  *string   `json:"self"`
		Out   []string  `json:"out"`
	} `json:"speakers"`
	Impersonation []struct {
		Response string          `json:"response"`
		Speakers []string        `json:"speakers"`
		Out      json.RawMessage `json:"out"`
	} `json:"impersonation"`
	StopError []struct {
		Message string `json:"message"`
		Out     bool   `json:"out"`
	} `json:"stop_error"`
	History []struct {
		Name           string            `json:"name"`
		NPC            string            `json:"npc"`
		Rows           []json.RawMessage `json:"rows"`
		Pruned         []int             `json:"pruned"`
		Messages       outcome           `json:"messages"`
		PrunedMessages outcome           `json:"pruned_messages"`
	} `json:"history"`
	Stabilize []struct {
		Name string            `json:"name"`
		Rows []json.RawMessage `json:"rows"`
		Cap  int               `json:"cap"`
		Kept int               `json:"kept"`
	} `json:"stabilize"`
	TurnContext []struct {
		Sim    bool    `json:"sim"`
		People string  `json:"people"`
		Eph    *string `json:"eph"`
		Out    string  `json:"out"`
	} `json:"turn_context"`
	Relative []struct {
		Now    float64 `json:"now"`
		SentAt string  `json:"sent_at"`
		Out    string  `json:"out"`
	} `json:"relative"`
	UserMessage []struct {
		Name            string            `json:"name"`
		History         []json.RawMessage `json:"history"`
		From            string            `json:"from"`
		Latest          *string           `json:"latest"`
		LatestUndefined bool              `json:"latest_undefined"`
		Out             string            `json:"out"`
	} `json:"user_message"`
	Prompts []struct {
		Agent      json.RawMessage `json:"agent"`
		Rag        string          `json:"rag"`
		Soul       string          `json:"soul"`
		People     string          `json:"people"`
		Bootstrap  *string         `json:"bootstrap"`
		Direct     promptRec       `json:"direct"`
		Sim        promptRec       `json:"sim"`
		Reflection promptRec       `json:"reflection"`
		Court      promptRec       `json:"court"`
	} `json:"prompts"`
	Caps []struct {
		Agent string `json:"agent"`
		Cap   int    `json:"cap"`
	} `json:"caps"`
	Cache []struct {
		Name    string            `json:"name"`
		Ops     []json.RawMessage `json:"ops"`
		Results json.RawMessage   `json:"results"`
	} `json:"cache"`
	Chat []chatScenario `json:"chat"`
	// SQL is Node's text of the direct-chat query kinds, whitespace folded.
	SQL map[string]string `json:"sql"`
}

type promptRec struct {
	Static  string `json:"static"`
	Dynamic string `json:"dynamic"`
}

func loadChatGoldens(t *testing.T) *chatGoldens {
	t.Helper()
	raw, err := os.ReadFile("testdata/goldens_chat.json")
	if err != nil {
		t.Fatal(err)
	}
	var g chatGoldens
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return &g
}

// chatBaseConfig is what goldens_chat.js's BASE_CONFIG adds to goldens.js's.
var chatBaseConfig = map[string]string{
	"virtual_agent_learning_enabled":   "false",
	"virtual_agent_chat_history_hours": "72",
	"global_bootstrap":                 "Be kind.",
}

func parseJS(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	v, err := jsutil.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// rowFrom is a golden history row (loadDirectChatHistory's columns).
func rowFrom(t *testing.T, raw json.RawMessage) chatRow {
	t.Helper()
	var r chatRow
	o := parseJS(t, raw).(*jsutil.Object)
	dest := []any{&r.FromAgent, &r.ToAgent, &r.Message, &r.SentAt, &r.ToolCalls, &r.ToolCallID}
	if err := scanRow(o, dest); err != nil {
		t.Fatal(err)
	}
	if o.Value("tool_calls") == nil {
		r.ToolCalls = nil
	}
	return r
}

func rowsFrom(t *testing.T, raws []json.RawMessage) []chatRow {
	out := []chatRow{}
	for _, raw := range raws {
		out = append(out, rowFrom(t, raw))
	}
	return out
}

func outcomeOf(v any, err error) outcome {
	if err != nil {
		return errText(err)
	}
	s := jsutil.Marshal(v)
	return outcome{OK: &s}
}

func TestChatPureFunctionsMatchNode(t *testing.T) {
	g := loadChatGoldens(t)
	for _, c := range g.Wrap {
		content := ""
		if c.Args[3] != nil {
			content = *c.Args[3]
		}
		if got := wrapBlock(*c.Args[0], *c.Args[1], *c.Args[2], content); got != c.Out {
			t.Errorf("wrapBlock %q: got %q want %q", content, got, c.Out)
		}
	}
	for _, c := range g.Paraphrase {
		name := parseJS(t, c.Name)
		if c.NameUndefined {
			name = jsutil.Undefined
		}
		got := outcomeOf(paraphraseToolCall(name, parseJS(t, c.Input)))
		if jsonOf(got) != jsonOf(c.Out) {
			t.Errorf("paraphrase %s %s: got %s want %s", c.Name, c.Input, jsonOf(got), jsonOf(c.Out))
		}
	}
	for _, c := range g.Gap {
		ms := math.NaN()
		if c.Ms != nil {
			ms = *c.Ms
		}
		var got *string
		if s, ok := formatGapMarker(ms); ok {
			got = &s
		}
		if jsonOf(got) != jsonOf(c.Out) {
			t.Errorf("gap %v: got %s want %s", ms, jsonOf(got), jsonOf(c.Out))
		}
	}
	for _, c := range g.CoLocated {
		got := extractCoLocatedNames(c.Text)
		if got == nil {
			got = []string{}
		}
		if c.Out == nil {
			c.Out = []string{}
		}
		if jsonOf(got) != jsonOf(c.Out) {
			t.Errorf("co-located %q: got %s want %s", c.Text, jsonOf(got), jsonOf(c.Out))
		}
	}
	for _, c := range g.Strip {
		if got := stripSimToolInstructions(c.Text); got != c.Out {
			t.Errorf("strip %q: got %q want %q", c.Text, got, c.Out)
		}
	}
	for _, c := range g.Speakers {
		var names []string
		for _, n := range c.Names {
			if n != nil {
				names = append(names, *n)
			} else {
				names = append(names, "")
			}
		}
		self := ""
		if c.Self != nil {
			self = *c.Self
		}
		got := collectDistinctSpeakers(names, self)
		if got == nil {
			got = []string{}
		}
		if jsonOf(got) != jsonOf(c.Out) {
			t.Errorf("speakers %v: got %s want %s", names, jsonOf(got), jsonOf(c.Out))
		}
	}
	for _, c := range g.Impersonation {
		r := scanForImpersonation(c.Response, c.Speakers)
		o := jsutil.NewObject()
		o.Set("text", r.Text)
		o.Set("truncated", r.Truncated)
		if r.Truncated {
			o.Set("rule", r.Rule)
		}
		o.Set("droppedChars", r.DroppedChars)
		if got, want := jsutil.Marshal(o), jsutil.Marshal(parseJS(t, c.Out)); got != want {
			t.Errorf("impersonation %q %v: got %s want %s", c.Response, c.Speakers, got, want)
		}
	}
	for _, c := range g.StopError {
		if got := isStopUnsupportedError(errors.New(c.Message)); got != c.Out {
			t.Errorf("stop error %q: got %v want %v", c.Message, got, c.Out)
		}
	}
	for _, c := range g.TurnContext {
		eph := ""
		if c.Eph != nil {
			eph = *c.Eph
		}
		if got := buildCurrentTurnContext(c.Sim, c.People, eph); got != c.Out {
			t.Errorf("turn context %v %q %q: got %q want %q", c.Sim, c.People, eph, got, c.Out)
		}
	}
	for _, c := range g.Relative {
		ts, _ := time.Parse(time.RFC3339Nano, c.SentAt)
		if got := formatRelativeTime(c.Now, ts); got != c.Out {
			t.Errorf("relative %s: got %q want %q", c.SentAt, got, c.Out)
		}
	}
	for _, c := range g.Caps {
		if got := historyCapFor(c.Agent); got != c.Cap {
			t.Errorf("cap %s: got %d want %d", c.Agent, got, c.Cap)
		}
	}
}

func TestChatHistoryTransformsMatchNode(t *testing.T) {
	g := loadChatGoldens(t)
	for _, c := range g.History {
		t.Run(c.Name, func(t *testing.T) {
			rows := rowsFrom(t, c.Rows)
			pruned := pruneSimHistory(rows)
			var idx []int
			for _, p := range pruned {
				for i := range rows {
					if jsonOf(rows[i]) == jsonOf(p) {
						idx = append(idx, i)
						break
					}
				}
			}
			if jsonOf(idx) != jsonOf(c.Pruned) {
				t.Errorf("pruned: got %v want %v", idx, c.Pruned)
			}
			build := func(rows []chatRow) outcome {
				msgs, err := buildToolUseMessages(rows, c.NPC)
				if err != nil {
					return errText(err)
				}
				return outcomeOf(objects(trimHeadToUserMessage(msgs)), nil)
			}
			if got := build(rows); jsonOf(got) != jsonOf(c.Messages) {
				t.Errorf("messages:\n got %s\nwant %s", jsonOf(got), jsonOf(c.Messages))
			}
			if got := build(pruned); jsonOf(got) != jsonOf(c.PrunedMessages) {
				t.Errorf("pruned messages:\n got %s\nwant %s", jsonOf(got), jsonOf(c.PrunedMessages))
			}
		})
	}
	for _, c := range g.Stabilize {
		if got := len(stabilizeHistoryWindow(rowsFrom(t, c.Rows), c.Cap)); got != c.Kept {
			t.Errorf("stabilize %s: kept %d want %d", c.Name, got, c.Kept)
		}
	}
}

func TestChatPromptsMatchNode(t *testing.T) {
	g := loadChatGoldens(t)
	for _, c := range g.UserMessage {
		s, _ := newService(t, &scenario{})
		latest := "undefined"
		if !c.LatestUndefined {
			latest = *c.Latest
		}
		if got := s.buildDirectChatUserMessage(rowsFrom(t, c.History), c.From, latest); got != c.Out {
			t.Errorf("user message %s:\n got %q\nwant %q", c.Name, got, c.Out)
		}
	}
	for i, c := range g.Prompts {
		s, _ := newService(t, &scenario{})
		cfg := s.Config.(mapConfig)
		cfg["global_bootstrap"] = chatBaseConfig["global_bootstrap"]
		if c.Bootstrap != nil {
			cfg["global_bootstrap"] = *c.Bootstrap
		}
		a := agentFrom(t, c.Agent)
		check := func(name string, p providers.SystemPrompt, err error, want promptRec) {
			if err != nil {
				t.Fatalf("prompt %d %s: %v", i, name, err)
			}
			if p.Static != want.Static || p.Dynamic != want.Dynamic {
				t.Errorf("prompt %d %s:\n got %q / %q\nwant %q / %q", i, name, p.Static, p.Dynamic, want.Static, want.Dynamic)
			}
		}
		p, err := s.buildDirectChatSystemPrompt(a, c.Rag, c.Soul, c.People)
		check("direct", p, err, c.Direct)
		p, err = s.buildSimChatSystemPrompt(a, c.Rag, c.Soul, c.People)
		check("sim", p, err, c.Sim)
		p, err = s.buildSimReflectionSystemPrompt(a, c.Rag, c.Soul, c.People)
		check("reflection", p, err, c.Reflection)
		check("court", buildSimCourtSystemPrompt(a, c.People), nil, c.Court)
	}
}

func TestSceneCacheMatchesNode(t *testing.T) {
	for _, c := range loadChatGoldens(t).Cache {
		t.Run(c.Name, func(t *testing.T) {
			s, r := newService(t, &scenario{})
			var results []any
			for _, raw := range c.Ops {
				var op []json.RawMessage
				json.Unmarshal(raw, &op)
				var name string
				json.Unmarshal(op[0], &name)
				arg := func(i int) string {
					var v string
					json.Unmarshal(op[i], &v)
					return v
				}
				switch name {
				case "recall":
					results = append(results, s.recallSceneCoLocated(arg(1)))
				case "remember":
					var names []string
					json.Unmarshal(op[2], &names)
					s.rememberSceneCoLocated(arg(1), names)
					results = append(results, "ok")
				case "forget":
					s.forgetSceneCoLocated(arg(1))
					results = append(results, "ok")
				case "advance":
					var ms float64
					json.Unmarshal(op[1], &ms)
					r.advance(ms)
					results = append(results, "ok")
				case "fill", "fill-same":
					var n int
					json.Unmarshal(op[1], &n)
					for i := 0; i < n; i++ {
						id := "f" + jsutil.NumberString(float64(i))
						s.rememberSceneCoLocated(id, []string{"p" + jsutil.NumberString(float64(i))})
						if name == "fill" {
							r.advance(1)
						}
					}
					results = append(results, "ok")
				}
			}
			var want any
			json.Unmarshal(c.Results, &want)
			if jsonOf(results) != jsonOf(want) {
				t.Errorf("results:\n got %s\nwant %s", jsonOf(results), jsonOf(want))
			}
		})
	}
}

// chatHarness adds to the recorder what only direct chat calls: chatSend,
// recorded as a "chat-send" query, and a provider the Node stub refuses.
type chatHarness struct {
	r *recorder
}

func (h *chatHarness) Send(ctx context.Context, in chat.SendInput) (*chat.Sent, chat.PendingReply, error) {
	text := func(v *string) *string { return v }
	b := func(v bool) *string { s := strconv.FormatBool(v); return &s }
	to := jsonOf(in.ToAgents.Names)
	var calls *string
	if in.ToolCalls != nil {
		c := string(in.ToolCalls)
		calls = &c
	}
	params := []*string{&in.FromAgent, &to, text(in.Message), text(in.SceneID), text(in.SceneStructure), text(in.ConversationID), calls, b(in.IsError), b(in.AckOnInsert)}
	if _, err := h.r.record("chat-send", params, false); err != nil {
		return nil, nil, err
	}
	return &chat.Sent{}, nil, nil
}

// CreateProvider refuses "unknown-provider", as the Node stub does, and
// otherwise is the recorder's.
func (h *chatHarness) CreateProvider(provider, model, apiKey string, conf any) (providers.Call, error) {
	if provider == "unknown-provider" {
		return nil, errors.New("Unsupported provider: " + provider)
	}
	return h.r.CreateProvider(provider, model, apiKey, conf)
}

func (h *chatHarness) DecryptAPIKey(k string) (string, error) { return h.r.DecryptAPIKey(k) }

// directChatFrom is a golden input as chat.Service would pass it.
func directChatFrom(t *testing.T, in chatInput) chat.DirectChat {
	o := parseJS(t, json.RawMessage(in.Opts)).(*jsutil.Object)
	str := func(k string) *string {
		if s, ok := o.Value(k).(string); ok {
			return &s
		}
		return nil
	}
	d := chat.DirectChat{
		VirtualAgent: in.Agent, FromAgent: in.From, Message: in.Message, MessageIDs: in.MessageIDs,
		IsToolResultCall: o.Value("isToolResultCall") == true,
		SceneID:          str("sceneId"), SceneStructure: str("sceneStructure"), ConversationID: str("conversationId"),
		SimActorID: str("simActorId"), SimActorName: str("simActorName"), HistoryScope: str("historyScope"),
		EphemeralContext: str("ephemeralContext"), StableContext: str("stableContext"),
		AckReplyOnInsert: o.Value("ackReplyOnInsert") == true, NotifyUnanswerable: o.Value("notifyUnanswerable") == true,
	}
	if v, ok := o.Get("toolsOffered"); ok {
		d.ToolsOffered = json.RawMessage(jsutil.Marshal(v))
	}
	return d
}

// chatOutcome is an outcome as goldens_chat.js's errInfo records it.
func chatOutcome(reply json.RawMessage, err error) string {
	if err == nil {
		var ok any
		if reply != nil {
			ok = string(reply)
		}
		return jsonOf(map[string]any{"ok": ok})
	}
	o := jsutil.NewObject()
	o.Set("message", err.Error())
	var limited *chat.RateLimitedError
	var over *chat.OverBudgetError
	var api *apierr.Error
	switch {
	case errors.As(err, &limited):
		o.Set("status_code", 429)
		o.Set("code", "RATE_LIMITED")
		o.Set("resumes_in_seconds", *limited.ResumesInSeconds)
	case errors.As(err, &over):
		o.Set("status_code", 402)
		o.Set("code", "OVER_BUDGET")
	case errors.As(err, &api):
		o.Set("status_code", api.Status)
	}
	return `{"error":` + jsutil.Marshal(o) + `}`
}

// Work Go does in the background: the call log, the activity spinner's
// off-switch, the transcript and the learning extraction.
var backgroundQueries = map[string]bool{"call-insert": true, "active-off": true, "dream-mode": true}
var backgroundLogs = map[string]bool{"call-log-error": true, "transcript-log-failed": true, "learning-extraction-failed": true}

func compareChat(t *testing.T, want *chatScenario, r *recorder, got []string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		done := len(r.queries) >= len(want.Queries) && len(r.logs) >= len(want.Logs) && len(r.events) >= len(want.Events) &&
			len(r.notes) >= len(want.Notes) && len(r.calls) >= len(want.Calls)
		r.mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()

	var wantOutcomes []string
	for _, o := range want.Outcomes {
		v, _ := jsutil.Parse(o)
		wantOutcomes = append(wantOutcomes, jsutil.Marshal(v))
	}
	for i := range got {
		v, _ := jsutil.Parse([]byte(got[i]))
		got[i] = jsutil.Marshal(v)
	}
	if jsonOf(got) != jsonOf(wantOutcomes) {
		t.Errorf("outcomes:\n got %s\nwant %s", jsonOf(got), jsonOf(wantOutcomes))
	}

	// Rows Go writes in the background float: the call log, the spinner's
	// off-switch (timer-marked) and the transcript; in an unordered
	// scenario, everything.
	queryLane := func(i, cut int, q queryRec) string {
		if want.Unordered || q.Timer || backgroundQueries[q.Kind] {
			return floating
		}
		return ""
	}
	logLane := func(i, cut int, l logRec) string {
		if want.Unordered || backgroundLogs[l.Action] {
			return floating
		}
		return ""
	}
	compareLanes(t, "queries", r.queries, want.Queries, noTail[queryRec], lane(queryLane))
	compareLanes(t, "logs", r.logs, want.Logs, noTail[logRec], lane(logLane))
	// The spinner's off event comes from the background off-switch; every
	// other event is compared in order, and the spinner must go off only
	// after it came on.
	compareLanes(t, "events", r.events, want.Events, noTail[eventRec],
		lane(func(i, cut int, e eventRec) string {
			if strings.Contains(e.Data, `"active":false`) {
				return floating
			}
			return ""
		}))
	var on, off []bool
	for _, e := range r.events {
		on = append(on, strings.Contains(e.Data, `"active":true`))
		off = append(off, strings.Contains(e.Data, `"active":false`))
	}
	checkSpinner(t, "events", on, off)
	on, off = nil, nil
	for _, q := range r.queries {
		on = append(on, q.Kind == "active-on")
		off = append(off, q.Kind == "active-off")
	}
	checkSpinner(t, "queries", on, off)
	for _, c := range []struct {
		name      string
		got, want string
	}{
		{"timers", sortedJSON(r.timers), sortedJSON(want.Timers)},
		{"notes", sortedJSON(r.notes), sortedJSON(want.Notes)},
		{"created", jsonOf(orEmpty(r.created)), jsonOf(orEmpty(want.Created))},
		{"calls", jsonOf(orEmpty(r.calls)), jsonOf(orEmpty(want.Calls))},
		{"reads", jsonOf(orEmpty(r.reads)), jsonOf(orEmpty(want.Reads))},
		{"searches", jsonOf(orEmpty(r.searches)), jsonOf(orEmpty(want.Searches))},
		{"errors", jsonOf(orEmpty(r.errors)), jsonOf(orEmpty(want.Errors))},
	} {
		if c.got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, c.got, c.want)
		}
	}
}

func newChatService(t *testing.T, sc *chatScenario) (*Service, *recorder) {
	s, r := newService(t, &sc.scenario)
	cfg := s.Config.(mapConfig)
	for k, v := range chatBaseConfig {
		if _, set := sc.Config[k]; !set {
			cfg[k] = v
		}
	}
	h := &chatHarness{r: r}
	s.Chat = h
	s.Providers = h
	return s, r
}

// The direct-chat queries' SQL is Node's, whitespace aside: the stubs
// answer from the scenario's rows whatever the WHERE clause says, so a
// changed filter would pass the scenarios.
func TestDirectChatSQLMatchesNode(t *testing.T) {
	g := loadChatGoldens(t)
	seen := map[string]string{}
	for _, c := range g.Chat {
		s, r := newChatService(t, &c)
		for _, in := range c.Inputs {
			s.HandleDirectChat(context.Background(), directChatFrom(t, in))
		}
		time.Sleep(20 * time.Millisecond)
		r.mu.Lock()
		for k, v := range r.sql {
			seen[k] = v
		}
		r.mu.Unlock()
	}
	for _, kind := range []string{"chat-history", "chat-ack"} {
		want := g.SQL[kind]
		if want == "" {
			t.Fatalf("no Node SQL for %s", kind)
		}
		if got := strings.Join(strings.Fields(seen[kind]), " "); got != want {
			t.Errorf("%s SQL:\n got %s\nwant %s", kind, got, want)
		}
	}
}

func TestDirectChatMatchesNode(t *testing.T) {
	old := localZone
	localZone = time.FixedZone("IST", 5*3600+1800)
	defer func() { localZone = old }()
	for _, c := range loadChatGoldens(t).Chat {
		t.Run(c.Name, func(t *testing.T) {
			s, r := newChatService(t, &c)
			var got []string
			for _, in := range c.Inputs {
				reply, err := s.HandleDirectChat(context.Background(), directChatFrom(t, in))
				got = append(got, chatOutcome(reply, err))
				time.Sleep(20 * time.Millisecond)
			}
			compareChat(t, &c, r, got)
		})
	}
}

// While the provider call is open the spinner is on and nothing has turned
// it off; after the call it goes off. The recorder's onCall runs inside the
// call, so it holds the call open and sees the state at that moment.
func TestDirectChatSpinnerSpansTheCall(t *testing.T) {
	var sc *chatScenario
	for _, c := range loadChatGoldens(t).Chat {
		if c.Name == "companion long message no rag" {
			sc = &c
			break
		}
	}
	if sc == nil {
		t.Fatal("scenario missing")
	}
	s, r := newChatService(t, sc)
	inCall := 0
	r.onCall = func() {
		inCall++
		// An off-switch started too early runs on a goroutine; give it time
		// to land. The right one cannot start before the call returns.
		time.Sleep(50 * time.Millisecond)
		r.mu.Lock()
		defer r.mu.Unlock()
		on, off := false, false
		for _, q := range r.queries {
			on = on || q.Kind == "active-on"
			off = off || q.Kind == "active-off"
		}
		for _, e := range r.events {
			on = on || strings.Contains(e.Data, `"active":true`)
			off = off || strings.Contains(e.Data, `"active":false`)
		}
		if !on || off {
			t.Errorf("during the provider call: spinner on %v, turned off %v", on, off)
		}
	}
	if _, err := s.HandleDirectChat(context.Background(), directChatFrom(t, sc.Inputs[0])); err != nil {
		t.Fatal(err)
	}
	if inCall != 1 {
		t.Fatalf("provider called %d times", inCall)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		off := false
		for _, e := range r.events {
			off = off || strings.Contains(e.Data, `"active":false`)
		}
		r.mu.Unlock()
		if off {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the spinner never went off after the call")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
