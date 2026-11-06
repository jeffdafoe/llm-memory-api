package virtualagent

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/chat"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/discussion"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/system"
)

// The discussion goldens (testdata/goldens_discussion.js) run Node's
// handleVirtualAgent and the functions only it uses with the same stubs as
// goldens_chat.js, plus recorders for evaluateVote and discussionLeave.

func init() {
	for name, id := range map[string]int{"system": 1, "prudence": 7, "ezekiel-crane": 8, "wendy": 13, "home": 14} {
		actorIDs[name] = id
	}
}

type discussionScenario struct {
	scenario
	Unordered bool              `json:"unordered"`
	Drain     bool              `json:"drain"`
	Payloads  []string          `json:"payloads"`
	Outcomes  []json.RawMessage `json:"outcomes"`
	// Handled are the outcomes of the triggers that arrived during a call.
	Handled []json.RawMessage `json:"handled"`
}

type discussionShape struct {
	ID      int     `json:"id"`
	Topic   string  `json:"topic"`
	Context *string `json:"context"`
	Mode    string  `json:"mode"`
}

func (d discussionShape) row() *discussionRow {
	return &discussionRow{ID: d.ID, Topic: d.Topic, Status: "active", Mode: d.Mode, Context: d.Context}
}

type historyShape struct {
	ID        int     `json:"id"`
	FromAgent string  `json:"from_agent"`
	Message   *string `json:"message"`
}

func historyFrom(in []historyShape) []historyMessage {
	out := []historyMessage{}
	for _, m := range in {
		out = append(out, historyMessage{ID: m.ID, FromAgent: m.FromAgent, Message: m.Message})
	}
	return out
}

type discussionGoldens struct {
	Context []struct {
		Agent      string          `json:"agent"`
		Discussion discussionShape `json:"discussion"`
		Out        string          `json:"out"`
	} `json:"context"`
	Prompts []struct {
		Agent        string          `json:"agent"`
		Instructions *string         `json:"instructions"`
		Discussion   discussionShape `json:"discussion"`
		Rag          string          `json:"rag"`
		Soul         string          `json:"soul"`
		People       string          `json:"people"`
		Bootstrap    string          `json:"bootstrap"`
		Static       string          `json:"static"`
		Dynamic      string          `json:"dynamic"`
	} `json:"prompts"`
	User []struct {
		History  []historyShape `json:"history"`
		Trigger  string         `json:"trigger"`
		Question *string        `json:"question"`
		Out      string         `json:"out"`
	} `json:"user"`
	ParseVote []struct {
		Text string  `json:"text"`
		Out  *string `json:"out"`
	} `json:"parse_vote"`
	Scenarios []discussionScenario `json:"scenarios"`
	SQL       map[string]string    `json:"sql"`
}

func loadDiscussionGoldens(t *testing.T) *discussionGoldens {
	t.Helper()
	raw, err := os.ReadFile("testdata/goldens_discussion.json")
	if err != nil {
		t.Fatal(err)
	}
	var g discussionGoldens
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return &g
}

// discussionBaseConfig is what goldens_discussion.js's BASE_CONFIG adds to
// goldens.js's.
var discussionBaseConfig = map[string]string{
	"virtual_agent_learning_enabled":         "false",
	"virtual_agent_response_delay_seconds":   "0",
	"virtual_agent_response_stagger_seconds": "0",
	"global_bootstrap":                       "Be kind.",
}

func TestDiscussionPureFunctionsMatchNode(t *testing.T) {
	g := loadDiscussionGoldens(t)
	for _, c := range g.Context {
		if got := renderDiscussionContext(c.Agent, c.Discussion.row()); got != c.Out {
			t.Errorf("context %+v:\n got %q\nwant %q", c.Discussion, got, c.Out)
		}
	}
	for i, c := range g.Prompts {
		s, _ := newService(t, &scenario{})
		s.Config.(mapConfig)["global_bootstrap"] = c.Bootstrap
		a := &Agent{Name: c.Agent, StartupInstructions: c.Instructions}
		p, err := s.buildSystemPrompt(a, c.Discussion.row(), c.Rag, c.Soul, c.People)
		if err != nil {
			t.Fatal(err)
		}
		if p.Static != c.Static || p.Dynamic != c.Dynamic {
			t.Errorf("prompt %d:\n got %q / %q\nwant %q / %q", i, p.Static, p.Dynamic, c.Static, c.Dynamic)
		}
	}
	for i, c := range g.User {
		if got := buildUserMessage(historyFrom(c.History), c.Trigger, c.Question); got != c.Out {
			t.Errorf("user message %d:\n got %q\nwant %q", i, got, c.Out)
		}
	}
	for _, c := range g.ParseVote {
		var got *string
		if v := parseVoteResponse(c.Text); v != nil {
			o := jsutil.NewObject()
			o.Set("choice", v.choice)
			o.Set("reason", v.reason)
			s := jsutil.Marshal(o)
			got = &s
		} else {
			s := "null"
			got = &s
		}
		if jsonOf(got) != jsonOf(c.Out) {
			t.Errorf("parse vote %q: got %s want %s", c.Text, jsonOf(got), jsonOf(c.Out))
		}
	}
}

// discussionHarness records what only the discussion path calls: chatSend
// (kind "chat-send"), evaluateVote and discussionLeave.
type discussionHarness struct {
	chatHarness
}

func (h *discussionHarness) Send(ctx context.Context, in chat.SendInput) (*chat.Sent, chat.PendingReply, error) {
	to := "null"
	if in.ToAgents.Given {
		to = jsonOf(in.ToAgents.Names)
	}
	var id *string
	if in.DiscussionID != 0 {
		s := strconv.FormatInt(in.DiscussionID, 10)
		id = &s
	}
	isError := strconv.FormatBool(in.IsError)
	if _, err := h.r.record("chat-send", []*string{&in.FromAgent, &to, id, in.Message, &isError}, false); err != nil {
		return nil, nil, err
	}
	return &chat.Sent{}, nil, nil
}

func (h *discussionHarness) EvaluateVote(ctx context.Context, voteID discussion.ID) error {
	id := jsonOf(voteID)
	_, err := h.r.record("evaluate-vote", []*string{&id}, false)
	return err
}

func (h *discussionHarness) Leave(ctx context.Context, discussionID discussion.ID, agent string) (*discussion.LeaveResult, error) {
	id := jsonOf(discussionID)
	if _, err := h.r.record("discussion-leave", []*string{&id, &agent}, false); err != nil {
		return nil, err
	}
	return &discussion.LeaveResult{}, nil
}

func newDiscussionService(t *testing.T, sc *discussionScenario) (*Service, *recorder) {
	s, r := newService(t, &sc.scenario)
	cfg := s.Config.(mapConfig)
	for k, v := range discussionBaseConfig {
		if _, set := sc.Config[k]; !set {
			cfg[k] = v
		}
	}
	for _, k := range sc.Unset {
		delete(cfg, k)
	}
	h := &discussionHarness{chatHarness{r: r}}
	s.Chat = h
	s.Providers = h
	s.Discussions = h
	r.project = map[string][]string{"discussion-load": {"id", "topic", "status", "mode", "context"}}
	return s, r
}

func handleOutcome(err error) string {
	if err != nil {
		return jsonOf(map[string]string{"error": err.Error()})
	}
	return `{"ok":null}`
}

func normalizedOutcomes(raws []json.RawMessage) []string {
	out := []string{}
	for _, o := range raws {
		v, _ := jsutil.Parse(o)
		out = append(out, jsutil.Marshal(v))
	}
	return out
}

func compareDiscussion(t *testing.T, want *discussionScenario, r *recorder, got, handled []string) {
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

	for _, c := range []struct {
		name      string
		got, want []string
	}{
		{"outcomes", normalizedOutcomes(rawAll(got)), normalizedOutcomes(want.Outcomes)},
		{"handled", normalizedOutcomes(rawAll(handled)), normalizedOutcomes(want.Handled)},
	} {
		if jsonOf(c.got) != jsonOf(c.want) {
			t.Errorf("%s:\n got %s\nwant %s", c.name, jsonOf(c.got), jsonOf(c.want))
		}
	}

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

func rawAll(in []string) []json.RawMessage {
	out := []json.RawMessage{}
	for _, s := range in {
		out = append(out, json.RawMessage(s))
	}
	return out
}

// runDiscussion replays a scenario's triggers one after another, as the
// Node script did, with the triggers that arrive during a call handled
// inside it.
func runDiscussion(t *testing.T, s *Service, r *recorder, sc *discussionScenario) (got, handled []string) {
	var mu sync.Mutex
	r.onDuring = func(p json.RawMessage) {
		out := handleOutcome(s.HandleVirtualAgent(context.Background(), p, system.Meta{}))
		mu.Lock()
		handled = append(handled, out)
		mu.Unlock()
	}
	for _, p := range sc.Payloads {
		got = append(got, handleOutcome(s.HandleVirtualAgent(context.Background(), json.RawMessage(p), system.Meta{})))
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	return got, handled
}

func TestDiscussionTriggerMatchesNode(t *testing.T) {
	old := localZone
	localZone = time.FixedZone("IST", 5*3600+1800)
	defer func() { localZone = old }()
	for _, c := range loadDiscussionGoldens(t).Scenarios {
		t.Run(c.Name, func(t *testing.T) {
			s, r := newDiscussionService(t, &c)
			got, handled := runDiscussion(t, s, r, &c)
			compareDiscussion(t, &c, r, got, handled)
		})
	}
}

// The discussion queries' SQL is Node's, whitespace aside (the stubs answer
// whatever the WHERE clause says). Node read the discussion with SELECT *;
// Go names the columns it reads.
func TestDiscussionSQLMatchesNode(t *testing.T) {
	g := loadDiscussionGoldens(t)
	seen := map[string]string{}
	for _, c := range g.Scenarios {
		s, r := newDiscussionService(t, &c)
		runDiscussion(t, s, r, &c)
		time.Sleep(20 * time.Millisecond)
		r.mu.Lock()
		for k, v := range r.sql {
			seen[k] = v
		}
		r.mu.Unlock()
	}
	for _, kind := range []string{"participants-load", "discussion-history", "vote-question", "ballot-check", "ballot-insert"} {
		want := g.SQL[kind]
		if want == "" {
			t.Fatalf("no Node SQL for %s", kind)
		}
		if got := strings.Join(strings.Fields(seen[kind]), " "); got != want {
			t.Errorf("%s SQL:\n got %s\nwant %s", kind, got, want)
		}
	}
	if got, want := strings.Join(strings.Fields(seen["discussion-load"]), " "), "SELECT id, topic, status, mode, context FROM discussions WHERE id = $1"; got != want {
		t.Errorf("discussion-load SQL: got %s", got)
	}
}
