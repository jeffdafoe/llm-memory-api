package enrichment

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/virtualagent"
)

// The goldens are what Node's enrichment.js did for each scenario, with the
// same stubs this file builds (testdata/goldens.js): the agent lookups, the
// model calls and their options, the statements and their parameters, the
// log lines, in order, and the outcome.

type goldenEvent struct {
	Kind          string    `json:"kind"`
	Name          string    `json:"name,omitempty"`
	Subsystem     string    `json:"subsystem,omitempty"`
	Action        string    `json:"action,omitempty"`
	Details       string    `json:"details,omitempty"`
	Agent         string    `json:"agent,omitempty"`
	Keys          []string  `json:"keys,omitempty"`
	User          string    `json:"user,omitempty"`
	Context       string    `json:"context,omitempty"`
	SkipRateLimit *bool     `json:"skipRateLimit,omitempty"`
	SkipCostLimit *bool     `json:"skipCostLimit,omitempty"`
	SkipRetry     *bool     `json:"skipRetry,omitempty"`
	SQL           string    `json:"sql,omitempty"`
	Params        []*string `json:"params,omitempty"`
}

type goldenAgent struct {
	Provider *string `json:"provider"`
	Model    *string `json:"model"`
	APIKey   *string `json:"api_key"`
}

type goldenScenario struct {
	Name        string            `json:"name"`
	Config      map[string]string `json:"config"`
	Namespace   string            `json:"namespace"`
	Slug        string            `json:"slug"`
	Title       *string           `json:"title"`
	Content     string            `json:"content"`
	Meta        string            `json:"meta"`
	Agent       *goldenAgent      `json:"agent"`
	LoadError   *string           `json:"loadError"`
	InvokeError *string           `json:"invokeError"`
	UpdateError *string           `json:"updateError"`
	Text        string            `json:"text"`
	Cost        float64           `json:"cost"`
	Events      []goldenEvent     `json:"events"`
	Result      struct {
		OK    bool    `json:"ok"`
		Error *string `json:"error"`
	} `json:"result"`
}

type goldenMetadata struct {
	Raw   string  `json:"raw"`
	OK    *string `json:"ok"`
	Error *string `json:"error"`
}

type goldens struct {
	Scenarios []goldenScenario `json:"scenarios"`
	Metadata  []goldenMetadata `json:"metadata"`
}

func loadGoldens(t *testing.T) goldens {
	t.Helper()
	raw, err := os.ReadFile("testdata/goldens.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldens
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// goldenNow is the clock the goldens ran with.
var goldenNow = time.Date(2026, 11, 6, 17, 0, 0, 0, time.UTC)

// recorder is the scenario's database, logger and virtual-agent service.
type recorder struct {
	sc     *goldenScenario
	events []goldenEvent
}

type fakeConfig struct{ values map[string]string }

func (c fakeConfig) Get(key string) (string, error) {
	v, ok := c.values[key]
	if !ok {
		return "", errors.New(`Config key "` + key + `" not found. Was config.init() called?`)
	}
	return v, nil
}

func (r *recorder) Log(subsystem, action string, details logger.Fields) {
	b, err := jsonutil.Marshal(details)
	if err != nil {
		panic(err)
	}
	r.events = append(r.events, goldenEvent{Kind: "log", Subsystem: subsystem, Action: action, Details: string(b)})
}

func (r *recorder) LoadAgent(ctx context.Context, name string) (*virtualagent.Agent, error) {
	r.events = append(r.events, goldenEvent{Kind: "load", Name: name})
	if r.sc.LoadError != nil {
		return nil, errors.New(*r.sc.LoadError)
	}
	if r.sc.Agent == nil {
		return nil, nil
	}
	return &virtualagent.Agent{Name: name, Virtual: true, Provider: r.sc.Agent.Provider, Model: r.sc.Agent.Model, APIKey: r.sc.Agent.APIKey}, nil
}

func (r *recorder) Invoke(ctx context.Context, agentName string, opts virtualagent.InvokeOptions) (*virtualagent.InvokeResult, error) {
	if opts.SystemPrompt != nil || opts.Messages != nil || opts.Tools != nil {
		panic("enrichment passes only a user message")
	}
	skipRetry := !opts.Retry
	skipRate, skipCost := opts.SkipRateLimit, opts.SkipCostLimit
	r.events = append(r.events, goldenEvent{
		Kind: "invoke", Agent: agentName,
		Keys: []string{"userMessage", "skipRateLimit", "skipCostLimit", "skipRetry", "context"},
		User: opts.UserMessage, Context: opts.Context,
		SkipRateLimit: &skipRate, SkipCostLimit: &skipCost, SkipRetry: &skipRetry,
	})
	if r.sc.InvokeError != nil {
		return nil, errors.New(*r.sc.InvokeError)
	}
	text, err := jsutil.Parse([]byte(r.sc.Text))
	if err != nil {
		panic(err)
	}
	return &virtualagent.InvokeResult{Text: text, Cost: r.sc.Cost, FinishReason: "stop"}, nil
}

func (r *recorder) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	params := make([]*string, len(args))
	for i, a := range args {
		s := a.(string)
		params[i] = &s
	}
	r.events = append(r.events, goldenEvent{Kind: "query", SQL: sql, Params: params})
	if r.sc.UpdateError != nil {
		return pgconn.CommandTag{}, &pgconn.PgError{Severity: "ERROR", Code: "42P01", Message: *r.sc.UpdateError}
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (r *recorder) Query(context.Context, string, ...any) (pgx.Rows, error) { panic("Query") }
func (r *recorder) QueryRow(context.Context, string, ...any) pgx.Row        { panic("QueryRow") }
func (r *recorder) Begin(context.Context) (pgx.Tx, error)                   { panic("Begin") }

var spaceRun = regexp.MustCompile(`\s+`)

// surrogateEscape is JSON.stringify's escape of a surrogate: a high one,
// with the low one's escape when they make a pair, or a lone low one.
var surrogateEscape = regexp.MustCompile(`\\ud[89ab][0-9a-f]{2}(\\ud[c-f][0-9a-f]{2})?|\\ud[c-f][0-9a-f]{2}`)

// goLogText is Node's log details as Go's logger writes them: each lone
// surrogate escape is U+FFFD, which is what Go holds for the half of a pair
// a substring cut, and U+2028 and U+2029, which JSON.stringify writes raw,
// are escaped (go-server note, Known differences).
func goLogText(s string) string {
	s = surrogateEscape.ReplaceAllStringFunc(s, func(m string) string {
		if len(m) == 12 {
			return m
		}
		return "\uFFFD"
	})
	return strings.NewReplacer("\u2028", `\u2028`, "\u2029", `\u2029`).Replace(s)
}

func TestEnrichMatchesNode(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	saved := zone
	zone = ny
	t.Cleanup(func() { zone = saved })

	g := loadGoldens(t)
	if len(g.Scenarios) < 70 {
		t.Fatalf("only %d scenarios", len(g.Scenarios))
	}
	for i := range g.Scenarios {
		sc := &g.Scenarios[i]
		t.Run(sc.Name, func(t *testing.T) {
			r := &recorder{sc: sc}
			e := &Enricher{DB: r, Config: fakeConfig{sc.Config}, Agents: r, Log: r, Now: func() time.Time { return goldenNow }}
			// The callers pass Node's metadata as the JSON of the parsed
			// value, and a NULL title as the text it concatenated to.
			var meta []byte
			if sc.Meta != "null" {
				meta = []byte(sc.Meta)
			}
			title := "null"
			if sc.Title != nil {
				title = *sc.Title
			}
			err := e.Enrich(context.Background(), sc.Namespace, sc.Slug, title, sc.Content, meta)

			if sc.Result.Error != nil {
				if err == nil || err.Error() != *sc.Result.Error {
					t.Errorf("error: got %v, want %q", err, *sc.Result.Error)
				}
			} else if err != nil {
				t.Errorf("unexpected error %v", err)
			}

			if len(r.events) != len(sc.Events) {
				t.Fatalf("%d events, want %d:\ngot  %+v\nwant %+v", len(r.events), len(sc.Events), r.events, sc.Events)
			}
			for j, want := range sc.Events {
				got := r.events[j]
				if want.Kind == "query" {
					want.SQL = strings.TrimSpace(spaceRun.ReplaceAllString(want.SQL, " "))
					got.SQL = strings.TrimSpace(spaceRun.ReplaceAllString(got.SQL, " "))
				}
				if want.Kind == "log" {
					want.Details = goLogText(want.Details)
				}
				wb, _ := json.Marshal(want)
				gb, _ := json.Marshal(got)
				if string(wb) != string(gb) {
					t.Errorf("event %d:\ngot  %s\nwant %s", j, gb, wb)
				}
			}
		})
	}
}

func TestEnrichMetadataMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Metadata {
		got, err := notes.EnrichMetadata([]byte(c.Raw))
		if c.Error != nil {
			// A JSON string's text is Go's JSON error (Known differences);
			// for the rest, V8's text is kept.
			if err == nil {
				t.Errorf("%s: no error, want %q", c.Raw, *c.Error)
			} else if !strings.HasPrefix(c.Raw, `"`) && err.Error() != *c.Error {
				t.Errorf("%s: error %q, want %q", c.Raw, err, *c.Error)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Raw, err)
			continue
		}
		text := "null"
		if got != nil {
			v, err := jsutil.Parse(got)
			if err != nil {
				t.Fatalf("%s: %v", c.Raw, err)
			}
			text = jsutil.Marshal(v)
		}
		if text != *c.OK {
			t.Errorf("%s: got %s, want %s", c.Raw, text, *c.OK)
		}
	}
}
