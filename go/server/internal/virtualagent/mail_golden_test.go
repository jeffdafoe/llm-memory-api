package virtualagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/mail"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/memory"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
)

// The direct-mail goldens: handleDirectMail end to end, its prompts and
// helpers (testdata/goldens.js).

type readRec struct {
	Namespace string `json:"namespace"`
	Slug      string `json:"slug"`
}

type searchRec struct {
	Query     string  `json:"query"`
	Namespace string  `json:"namespace"`
	Limit     float64 `json:"limit"`
}

type mailGoldens struct {
	MailPrompt []struct {
		Agent  json.RawMessage   `json:"agent"`
		RAG    string            `json:"rag"`
		Soul   string            `json:"soul"`
		People string            `json:"people"`
		Config map[string]string `json:"config"`
		Unset  []string          `json:"unset"`
		Out    *struct {
			Static  string `json:"static"`
			Dynamic string `json:"dynamic"`
		} `json:"out"`
		Error string `json:"error"`
	} `json:"mail_prompt"`
	MailUser []struct {
		Mail struct {
			FromAgent string `json:"from_agent"`
			Subject   string `json:"subject"`
			Body      string `json:"body"`
		} `json:"mail"`
		Out string `json:"out"`
	} `json:"mail_user"`
	Duration []struct {
		In  float64 `json:"in"`
		Out string  `json:"out"`
	} `json:"duration"`
	Slug []struct {
		In  string  `json:"in"`
		Out *string `json:"out"`
	} `json:"slug"`
	Notice []struct {
		Agent   string   `json:"agent"`
		Reason  string   `json:"reason"`
		Missing []string `json:"missing"`
		Out     string   `json:"out"`
	} `json:"notice"`
	Missing []struct {
		Row json.RawMessage `json:"row"`
		Out []string        `json:"out"`
	} `json:"missing"`
	Mail []struct {
		scenario
		Agent  string `json:"agent"`
		From   string `json:"from"`
		MailID string `json:"mail_id"`
	} `json:"mail"`
}

func loadMailGoldens(t *testing.T) *mailGoldens {
	t.Helper()
	raw, err := os.ReadFile("testdata/goldens.json")
	if err != nil {
		t.Fatal(err)
	}
	var g mailGoldens
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return &g
}

// Read answers from the scenario's notes; any other slug is not found.
func (r *recorder) Read(ctx context.Context, namespace, slug string) (notes.Note, error) {
	r.mu.Lock()
	r.reads = append(r.reads, readRec{namespace, slug})
	r.mu.Unlock()
	content, ok := r.sc.NotesIn[slug]
	if !ok {
		return notes.Note{}, errors.New("Note not found: " + slug)
	}
	return notes.Note{Namespace: namespace, Slug: slug, Content: content}, nil
}

func (r *recorder) Search(ctx context.Context, in memory.SearchInput) (memory.SearchResults, error) {
	r.mu.Lock()
	r.searches = append(r.searches, searchRec{in.Query, in.Namespace, in.Limit})
	r.mu.Unlock()
	if r.sc.SearchError != "" {
		return memory.SearchResults{}, errors.New(r.sc.SearchError)
	}
	var res memory.SearchResults
	if len(r.sc.Search) > 0 && string(r.sc.Search) != "null" {
		if err := json.Unmarshal(r.sc.Search, &res); err != nil {
			r.t.Fatal(err)
		}
	}
	return res, nil
}

// Send records the mail as a "mail-send" query, so its place among the
// writes is compared.
func (r *recorder) Send(ctx context.Context, toAgent, fromAgent, subject, body, inReplyTo string) (*mail.Sent, error) {
	params := []*string{&toAgent, &fromAgent, &subject, &body, &inReplyTo}
	if _, err := r.record("mail-send", params, false); err != nil {
		return nil, err
	}
	return &mail.Sent{ID: "sent"}, nil
}

// LogError records what Node's logError stub kept: the error's fields
// (its stack aside), with the unset ones left out as JSON.stringify did.
func (r *recorder) LogError(subsystem, action string, opts logger.ErrorOpts) {
	o := jsutil.NewObject()
	for _, f := range []struct {
		key   string
		value string
	}{{"agent", opts.Agent}, {"context", opts.Context}, {"contextId", opts.ContextID}, {"message", opts.Message}} {
		if f.value != "" {
			o.Set(f.key, f.value)
		}
	}
	if opts.StatusCode != 0 {
		o.Set("statusCode", opts.StatusCode)
	}
	r.mu.Lock()
	r.errors = append(r.errors, logRec{subsystem, action, jsutil.Marshal(o)})
	r.mu.Unlock()
}

// floating is the lane of a write whose place against the others is not
// fixed: Go makes it from a goroutine, Node in a fixed microtask order. Such
// writes are compared as a multiset.
const floating = "floating"

// lanes splits a recording by lane: "" is the handler's own sequence, a
// named lane is one background job (learning, transcript), each compared in
// order, and floating is compared as a multiset. cut is where the jobs
// started after the reply (len(items) when they did not).
func lanes[T any](items []T, cut int, lane func(i, cut int, it T) string) map[string][]string {
	out := map[string][]string{}
	for i, it := range items {
		l := lane(i, cut, it)
		out[l] = append(out[l], jsonOf(it))
	}
	sort.Strings(out[floating])
	return out
}

// compareLanes compares each lane. newLane makes a fresh lane function for
// each side, as a lane may depend on what came before it.
func compareLanes[T any](t *testing.T, name string, got, want []T, cut func([]T) int, newLane func() func(i, cut int, it T) string) {
	t.Helper()
	g, w := lanes(got, cut(got), newLane()), lanes(want, cut(want), newLane())
	names := map[string]bool{}
	for k := range g {
		names[k] = true
	}
	for k := range w {
		names[k] = true
	}
	for k := range names {
		if a, b := strings.Join(g[k], "\n"), strings.Join(w[k], "\n"); a != b {
			t.Errorf("%s, lane %q:\n got %s\nwant %s", name, k, a, b)
		}
	}
}

// cutAfter is the index after the last item that is, or len(items) when
// none is.
func cutAfter[T any](is func(T) bool) func([]T) int {
	return func(items []T) int {
		for i := len(items) - 1; i >= 0; i-- {
			if is(items[i]) {
				return i + 1
			}
		}
		return len(items)
	}
}

func isMailSend(q queryRec) bool { return q.Kind == "mail-send" }

// queryLane places a query: the call logs and the mail call's spinner
// off-switch (Timer: it goes off from a timer, which in Go can land anywhere,
// even inside the learning job) float; before the cut is the handler's own
// sequence; after it, the transcript's read, else the learning job (whose own
// off-switch is not a timer's).
func queryLane(i, cut int, q queryRec) string {
	switch {
	case q.Kind == "call-insert", q.Timer:
		return floating
	case i < cut:
		return ""
	case q.Kind == "dream-mode":
		return "transcript"
	}
	return "learning"
}

// lane is newLane for a lane function with no state.
func lane[T any](f func(i, cut int, it T) string) func() func(i, cut int, it T) string {
	return func() func(i, cut int, it T) string { return f }
}

// noTail puts every item before the cut.
func noTail[T any](items []T) int { return len(items) }

// checkSpinner fails when the activity spinner goes off more often than it
// came on at any point, or ends on: an off-switch the comparison above lets
// float must still follow its own on-switch.
func checkSpinner(t *testing.T, name string, on, off []bool) {
	t.Helper()
	n := 0
	for i := range on {
		if on[i] {
			n++
		}
		if off[i] {
			n--
		}
		if n < 0 {
			t.Errorf("%s: the spinner goes off before it came on (item %d)", name, i)
			return
		}
	}
	if n != 0 {
		t.Errorf("%s: the spinner is left on (%d)", name, n)
	}
}

// compareMail is compare for handleDirectMail. Node finished its background
// work in a fixed microtask order; Go runs it on goroutines. The handler's
// own writes are compared in order, and so is each background job after the
// reply (learning extraction, transcript), though the jobs may interleave;
// the call logs and the activity spinner's off-switch float, and the
// spinner is checked to go off only after it came on.
func compareMail(t *testing.T, want *scenario, r *recorder, got outcome) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		r.mu.Lock()
		done := len(r.queries) >= len(want.Queries) && len(r.logs) >= len(want.Logs) &&
			len(r.events) >= len(want.Events) && len(r.notes) >= len(want.Notes)
		r.mu.Unlock()
		if done || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	r.mu.Lock()
	defer r.mu.Unlock()

	if jsonOf(got) != jsonOf(want.Outcome) {
		t.Errorf("outcome:\n got %s\nwant %s", jsonOf(got), jsonOf(want.Outcome))
	}

	compareLanes(t, "queries", r.queries, want.Queries, cutAfter(isMailSend), lane(queryLane))
	compareLanes(t, "logs", r.logs, want.Logs, cutAfter(func(l logRec) bool { return l.Action == "direct-mail-responded" }),
		lane(func(i, cut int, l logRec) string {
			switch {
			case l.Action == "call-log-error":
				return floating
			case i < cut:
				return ""
			case l.Action == "transcript-log-failed":
				return "transcript"
			}
			return "learning"
		}))
	compareLanes(t, "events", r.events, want.Events, noTail[eventRec],
		lane(func(i, cut int, e eventRec) string {
			if strings.Contains(e.Data, `"active":false`) {
				return floating
			}
			return ""
		}))
	compareLanes(t, "notes", r.notes, want.Notes, noTail[noteRec],
		lane(func(i, cut int, n noteRec) string {
			if strings.HasPrefix(n.Slug, "conversations/") {
				return "transcript"
			}
			return "learning"
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

func sortedJSON[T any](items []T) string {
	var out []string
	for _, it := range items {
		out = append(out, jsonOf(it))
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

func orEmpty[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

func TestDirectMailMatchesNode(t *testing.T) {
	old := localZone
	localZone = time.FixedZone("IST", 5*3600+1800)
	defer func() { localZone = old }()
	for _, c := range loadMailGoldens(t).Mail {
		t.Run(c.Name, func(t *testing.T) {
			s, r := newService(t, &c.scenario)
			err := s.HandleDirectMail(context.Background(), c.Agent, c.From, c.MailID)
			got := outcome{}
			if err != nil {
				got = errText(err)
			}
			compareMail(t, &c.scenario, r, got)
		})
	}
}

func TestMailPromptMatchesNode(t *testing.T) {
	for _, c := range loadMailGoldens(t).MailPrompt {
		sc := &scenario{Config: c.Config, Unset: c.Unset}
		s, _ := newService(t, sc)
		p, err := s.buildMailSystemPrompt(agentFrom(t, c.Agent), c.RAG, c.Soul, c.People)
		if c.Out == nil {
			if err == nil || err.Error() != c.Error {
				t.Errorf("%v: got %v, want error %q", c.Unset, err, c.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: %v", c.Config, err)
			continue
		}
		if p.Static != c.Out.Static || p.Dynamic != c.Out.Dynamic {
			t.Errorf("%v:\n got %q / %q\nwant %q / %q", c.Config, p.Static, p.Dynamic, c.Out.Static, c.Out.Dynamic)
		}
	}
	for _, c := range loadMailGoldens(t).MailUser {
		if got := buildMailUserMessage(c.Mail.FromAgent, c.Mail.Subject, c.Mail.Body); got != c.Out {
			t.Errorf("user message: got %q want %q", got, c.Out)
		}
	}
}

func TestMailHelpersMatchNode(t *testing.T) {
	g := loadMailGoldens(t)
	for _, c := range g.Duration {
		if got := formatDuration(c.In); got != c.Out {
			t.Errorf("formatDuration(%v) = %q, want %q", c.In, got, c.Out)
		}
	}
	for _, c := range g.Slug {
		want := ""
		if c.Out != nil {
			want = *c.Out
		}
		if got := personContextSlug(c.In); got != want {
			t.Errorf("personContextSlug(%q) = %q, want %q", c.In, got, want)
		}
	}
	for _, c := range g.Notice {
		if got := unanswerableNotice(c.Agent, c.Reason, c.Missing); got != c.Out {
			t.Errorf("notice %s %v: got %q want %q", c.Reason, c.Missing, got, c.Out)
		}
		if got := (&Service{}).UnanswerableNotice(c.Agent, c.Reason, c.Missing); got != c.Out {
			t.Errorf("Service notice %s: got %q", c.Reason, got)
		}
	}
	for _, c := range g.Missing {
		if got := missingConfigParts(agentFrom(t, c.Row)); jsonOf(orEmpty(got)) != jsonOf(orEmpty(c.Out)) {
			t.Errorf("missing %s: got %v want %v", c.Row, got, c.Out)
		}
	}
}

// The query lanes tell the mail call's spinner off-switch from the learning
// job's own by its timer mark, not by where it lands.
func TestMailQueryLanes(t *testing.T) {
	seven := "7"
	q := func(kind string, timer bool) queryRec { return queryRec{kind, []*string{&seven}, timer} }
	lanesOf := func(items []queryRec) string {
		return jsonOf(lanes(items, cutAfter(isMailSend)(items), queryLane))
	}
	node := []queryRec{
		q("active-on", false), q("active-off", true), q("mail-send", false),
		q("active-on", false), q("active-off", false), q("usage-insert", false),
	}
	// The mail off-switch lands inside the learning job, whose own writes
	// keep their order: the same lanes.
	late := []queryRec{
		q("active-on", false), q("mail-send", false),
		q("active-on", false), q("active-off", true), q("active-off", false), q("usage-insert", false),
	}
	if lanesOf(late) != lanesOf(node) {
		t.Errorf("a late mail off-switch changed the lanes:\n got %s\nwant %s", lanesOf(late), lanesOf(node))
	}
	// The learning job's off-switch and usage write reversed, with the mail
	// off-switch between them: must differ.
	reversed := []queryRec{
		q("active-on", false), q("mail-send", false),
		q("active-on", false), q("active-off", true), q("usage-insert", false), q("active-off", false),
	}
	if lanesOf(reversed) == lanesOf(node) {
		t.Errorf("a reversed learning job compares equal: %s", lanesOf(reversed))
	}
}
