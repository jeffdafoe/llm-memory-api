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
	if _, err := r.record("mail-send", params); err != nil {
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

// split cuts a recording into the part compared in order and the part
// compared as a multiset: items floating marks (written in the background,
// so their place against the rest is not fixed), and everything from the
// cut on (the learning extraction and transcript, which run in parallel
// after the reply). cut is len(items) when there is no tail.
func split[T any](items []T, cut int, floating func(T) bool) (ordered []string, unordered []string) {
	for i, it := range items {
		if i >= cut || floating(it) {
			unordered = append(unordered, jsonOf(it))
		} else {
			ordered = append(ordered, jsonOf(it))
		}
	}
	sort.Strings(unordered)
	return ordered, unordered
}

// cutAfter is the index after the last item that is, or len(items) when
// none is.
func cutAfter[T any](items []T, is func(T) bool) int {
	for i := len(items) - 1; i >= 0; i-- {
		if is(items[i]) {
			return i + 1
		}
	}
	return len(items)
}

func sortedJSON[T any](items []T) string {
	var out []string
	for _, it := range items {
		out = append(out, jsonOf(it))
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// compareMail is compare for handleDirectMail. Node finished its background
// work in a fixed microtask order; Go runs it on goroutines. So the call
// log, the spinner's off-switch and everything after the reply (learning,
// transcript) are compared as multisets, and the rest in order.
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
	isSend := func(q queryRec) bool { return q.Kind == "mail-send" }
	floatingQuery := func(q queryRec) bool { return q.Kind == "call-insert" || q.Kind == "active-off" }
	responded := func(l logRec) bool { return l.Action == "direct-mail-responded" }
	floatingLog := func(l logRec) bool { return l.Action == "call-log-error" }
	floatingEvent := func(e eventRec) bool { return strings.Contains(e.Data, `"active":false`) }
	everything := func(n int) int { return n }

	for _, c := range []splitResult{
		queries(r.queries, want.Queries, isSend, floatingQuery),
		logsOf(r.logs, want.Logs, responded, floatingLog),
		eventsOf(r.events, want.Events, everything, floatingEvent),
	} {
		if g, w := strings.Join(c.gotOrdered, "\n"), strings.Join(c.wantOrdered, "\n"); g != w {
			t.Errorf("%s in order:\n got %s\nwant %s", c.name, g, w)
		}
		if g, w := strings.Join(c.gotRest, "\n"), strings.Join(c.wantRest, "\n"); g != w {
			t.Errorf("%s in the background:\n got %s\nwant %s", c.name, g, w)
		}
	}
	for _, c := range []struct {
		name      string
		got, want string
	}{
		{"notes", sortedJSON(r.notes), sortedJSON(want.Notes)},
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

func orEmpty[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

type splitResult = struct {
	name                  string
	gotOrdered, gotRest   []string
	wantOrdered, wantRest []string
}

func queries(got, want []queryRec, cut func(queryRec) bool, floating func(queryRec) bool) splitResult {
	go1, gr := split(got, cutAfter(got, cut), floating)
	wo, wr := split(want, cutAfter(want, cut), floating)
	return splitResult{"queries", go1, gr, wo, wr}
}

func logsOf(got, want []logRec, cut func(logRec) bool, floating func(logRec) bool) splitResult {
	go1, gr := split(got, cutAfter(got, cut), floating)
	wo, wr := split(want, cutAfter(want, cut), floating)
	return splitResult{"logs", go1, gr, wo, wr}
}

func eventsOf(got, want []eventRec, cut func(int) int, floating func(eventRec) bool) splitResult {
	go1, gr := split(got, cut(len(got)), floating)
	wo, wr := split(want, cut(len(want)), floating)
	return splitResult{"events", go1, gr, wo, wr}
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
		if got := (Seam{}).UnanswerableNotice(c.Agent, c.Reason, c.Missing); got != c.Out {
			t.Errorf("Seam notice %s: got %q", c.Reason, got)
		}
	}
	for _, c := range g.Missing {
		if got := missingConfigParts(agentFrom(t, c.Row)); jsonOf(orEmpty(got)) != jsonOf(orEmpty(c.Out)) {
			t.Errorf("missing %s: got %v want %v", c.Row, got, c.Out)
		}
	}
}
