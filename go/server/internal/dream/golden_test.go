package dream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/peopleslug"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sim"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/virtualagent"
)

// goldens is testdata/goldens.json, written by testdata/goldens.js from
// Node's dream.js. Regenerate it with that script when the Node side
// changes.
type goldens struct {
	Constants struct {
		Signals         [][2]string `json:"signals"`
		Markers         []string    `json:"markers"`
		LedgerDirective string      `json:"ledgerDirective"`
		SoulLength      string      `json:"soulLength"`
	} `json:"constants"`
	Prefilter []struct {
		Content  string  `json:"content"`
		Filtered *string `json:"filtered"`
	} `json:"prefilter"`
	Speakers []struct {
		Content  string              `json:"content"`
		Agent    string              `json:"agent"`
		Speakers [][]json.RawMessage `json:"speakers"`
	} `json:"speakers"`
	Sections []struct {
		Filtered string              `json:"filtered"`
		Self     string              `json:"self"`
		Sections [][]json.RawMessage `json:"sections"`
	} `json:"sections"`
	PersonMessages []struct {
		In      map[string]json.RawMessage `json:"in"`
		Message string                     `json:"message"`
	} `json:"personMessages"`
	SoulMessages []struct {
		In      map[string]json.RawMessage `json:"in"`
		Message string                     `json:"message"`
	} `json:"soulMessages"`
	NotesLogs []struct {
		Rows []struct {
			Slug      string `json:"slug"`
			Content   string `json:"content"`
			UpdatedAt string `json:"updated_at"`
		} `json:"rows"`
		Log string `json:"log"`
	} `json:"notesLogs"`
	Chunks []struct {
		Since  string      `json:"since"`
		Now    string      `json:"now"`
		Chunks [][2]string `json:"chunks"`
	} `json:"chunks"`
	Slugify        [][2]string         `json:"slugify"`
	Titles         [][2]string         `json:"titles"`
	LedgerSpeakers [][]json.RawMessage `json:"ledgerSpeakers"`
	JSONHeads      [][]json.RawMessage `json:"jsonHeads"`
	Names          [][]json.RawMessage `json:"names"`
	Preambles      [][]json.RawMessage `json:"preambles"`
	Rebuild        [][]json.RawMessage `json:"rebuild"`
	Prefixes       []struct {
		In       json.RawMessage `json:"in"`
		Resolved attempt         `json:"resolved"`
		Roster   *string         `json:"roster"`
	} `json:"prefixes"`
	PeoplePaths []struct {
		Prefix json.RawMessage `json:"prefix"`
		Slug   string          `json:"slug"`
		Path   attempt         `json:"path"`
		Valid  *string         `json:"valid"`
	} `json:"peoplePaths"`
	Slugs        [][2]*string `json:"slugs"`
	SlugPrefixes [][3]string  `json:"slugPrefixes"`
	Displays     [][2]string  `json:"displays"`
	Delays       []struct {
		Value    *string `json:"value"`
		Fallback float64 `json:"fallback"`
		Result   struct {
			Value *float64 `json:"value"`
			Error string   `json:"error"`
		} `json:"result"`
	} `json:"delays"`
	Passes []passGolden `json:"passes"`
	Starts []struct {
		Schedule *string          `json:"schedule"`
		Started  bool             `json:"started"`
		Threw    string           `json:"threw"`
		Events   []map[string]any `json:"events"`
	} `json:"starts"`
}

type attempt struct {
	Value *string `json:"value"`
	Error string  `json:"error"`
}

type passGolden struct {
	Name          string               `json:"name"`
	Clock         string               `json:"clock"`
	Config        map[string]string    `json:"config"`
	ConnectError  string               `json:"connectError"`
	Trusted       []int                `json:"trusted"`
	TrustError    string               `json:"trustError"`
	NotesIn       map[string]string    `json:"notesIn"`
	Lists         map[string][]string  `json:"lists"`
	FailNth       map[string][]*string `json:"failNth"`
	Answers       []answer             `json:"answers"`
	Script        []scriptStep         `json:"script"`
	Unused        int                  `json:"unused"`
	UnusedAnswers int                  `json:"unusedAnswers"`
	Events        []map[string]any     `json:"events"`
}

type answer struct {
	Text      json.RawMessage `json:"text"`
	Truncated bool            `json:"truncated"`
	Finish    string          `json:"finish"`
	Usage     json.RawMessage `json:"usage"`
	Error     string          `json:"error"`
}

type scriptStep struct {
	Match   string   `json:"match"`
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	Error   string   `json:"error"`
	Lose    string   `json:"lose"`
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

func unmarshal[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return v
}

func TestConstantsMatchNode(t *testing.T) {
	g := loadGoldens(t)
	if len(g.Constants.Signals) != len(signalSources) {
		t.Fatalf("%d signal patterns, Node has %d", len(signalSources), len(g.Constants.Signals))
	}
	for i, s := range g.Constants.Signals {
		if s[0] != signalSources[i] || s[1] != "i" {
			t.Errorf("signal %d: %q, Node has /%s/%s", i, signalSources[i], s[0], s[1])
		}
	}
	if !reflect.DeepEqual(g.Constants.Markers, reasoningPreambleMarkers) {
		t.Errorf("markers %q, Node has %q", reasoningPreambleMarkers, g.Constants.Markers)
	}
	if g.Constants.LedgerDirective != ledgerPrecedenceDirective {
		t.Errorf("ledger directive differs:\n got %q\nwant %q", ledgerPrecedenceDirective, g.Constants.LedgerDirective)
	}
	if g.Constants.SoulLength != sim.SoulLengthDirective {
		t.Errorf("soul length directive differs")
	}
}

func TestPrefilterMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Prefilter {
		got, ok := prefilterLog(c.Content)
		if c.Filtered == nil {
			if ok {
				t.Errorf("%q: kept %q, Node kept nothing", c.Content, got)
			}
			continue
		}
		if !ok || got != *c.Filtered {
			t.Errorf("%q:\n got %q (%v)\nwant %q", c.Content, got, ok, *c.Filtered)
		}
	}
}

func speakerTuples(sp *speakers) [][]any {
	out := [][]any{}
	for _, s := range sp.all() {
		lines := s.lines
		if lines == nil {
			lines = []string{}
		}
		out = append(out, []any{s.slug, s.display, lines})
	}
	return out
}

// sameJSON compares a Go value with Node's JSON by their encodings.
func sameJSON(t *testing.T, got any, want any) bool {
	t.Helper()
	g, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	w, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var gv, wv any
	_ = json.Unmarshal(g, &gv)
	_ = json.Unmarshal(w, &wv)
	return reflect.DeepEqual(gv, wv)
}

func TestExtractSpeakersMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Speakers {
		got := speakerTuples(extractSpeakers(c.Content, c.Agent))
		if !sameJSON(t, got, c.Speakers) {
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(c.Speakers)
			t.Errorf("agent %q over %.40q:\n got %s\nwant %s", c.Agent, c.Content, gb, wb)
		}
	}
}

func TestPersonSectionsMatchNode(t *testing.T) {
	for _, c := range loadGoldens(t).Sections {
		var got [][]any
		for _, s := range buildPersonExcerptSections(c.Filtered, c.Self, extractSpeakers(c.Filtered, c.Self)) {
			ledger, said := s.ledger, s.said
			if ledger == nil {
				ledger = []string{}
			}
			if said == nil {
				said = []string{}
			}
			got = append(got, []any{s.slug, s.display, ledger, said})
		}
		if got == nil {
			got = [][]any{}
		}
		if !sameJSON(t, got, c.Sections) {
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(c.Sections)
			t.Errorf("self %q:\n got %s\nwant %s", c.Self, gb, wb)
		}
	}
}

func optString(t *testing.T, in map[string]json.RawMessage, key string) string {
	raw, ok := in[key]
	if !ok {
		return ""
	}
	var s *string
	if json.Unmarshal(raw, &s) != nil || s == nil {
		return ""
	}
	return *s
}

func TestPromptMessagesMatchNode(t *testing.T) {
	g := loadGoldens(t)
	for i, c := range g.PersonMessages {
		var ledger []string
		if raw, ok := c.In["ledger"]; ok {
			_ = json.Unmarshal(raw, &ledger)
		}
		got := buildPersonUserMessage(personMessage{
			selfLabel: optString(t, c.In, "selfLabel"), display: optString(t, c.In, "display"),
			today: optString(t, c.In, "today"), existingFile: optString(t, c.In, "existingFile"),
			ledger: ledger, said: optString(t, c.In, "said"),
		})
		if got != c.Message {
			t.Errorf("person message %d:\n got %q\nwant %q", i, got, c.Message)
		}
	}
	for i, c := range g.SoulMessages {
		var rebuild bool
		_ = json.Unmarshal(c.In["needsRebuild"], &rebuild)
		got := buildSoulUserMessage(soulMessage{
			agentName: optString(t, c.In, "agentName"), startupInstructions: optString(t, c.In, "startupInstructions"),
			existingSoul: optString(t, c.In, "existingSoul"), needsRebuild: rebuild,
			backloadDreams: optString(t, c.In, "backloadDreams"), chunkDate: optString(t, c.In, "chunkDate"),
			dreamContent: optString(t, c.In, "dreamContent"),
		})
		if got != c.Message {
			t.Errorf("soul message %d:\n got %q\nwant %q", i, got, c.Message)
		}
	}
}

func pgTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse("2006-01-02 15:04:05.999999999-07", s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestTextHelpersMatchNode(t *testing.T) {
	g := loadGoldens(t)
	for _, c := range g.NotesLogs {
		var rows []noteRow
		for _, r := range c.Rows {
			rows = append(rows, noteRow{slug: r.Slug, content: r.Content, updatedAt: pgTime(t, r.UpdatedAt)})
		}
		if got := buildNotesLog(rows); got != c.Log {
			t.Errorf("notes log:\n got %q\nwant %q", got, c.Log)
		}
	}
	for _, c := range g.Chunks {
		since, _ := time.Parse(time.RFC3339Nano, c.Since)
		now, _ := time.Parse(time.RFC3339Nano, c.Now)
		got := [][2]string{}
		for _, ch := range computeDailyChunks(since, now) {
			got = append(got, [2]string{jsutil.DateISO(ch.from), jsutil.DateISO(ch.to)})
		}
		want := c.Chunks
		if want == nil {
			want = [][2]string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("chunks %s..%s: %v, Node %v", c.Since, c.Now, got, want)
		}
	}
	for _, c := range g.Slugify {
		if got := slugify(c[0]); got != c[1] {
			t.Errorf("slugify(%q) = %q, Node %q", c[0], got, c[1])
		}
	}
	for _, c := range g.Titles {
		if got := dreamTitle(c[0]); got != c[1] {
			t.Errorf("title of %q = %q, Node %q", c[0], got, c[1])
		}
	}
	for _, c := range g.LedgerSpeakers {
		line := unmarshal[string](t, c[0])
		if got, want := ledgerLineSpeaker(line), unmarshal[string](t, c[1]); got != want {
			t.Errorf("ledgerLineSpeaker(%q) = %q, Node %q", line, got, want)
		}
		if got, want := ledgerLine(line), unmarshal[bool](t, c[2]); got != want {
			t.Errorf("ledgerLine(%q) = %v, Node %v", line, got, want)
		}
	}
	for _, c := range g.JSONHeads {
		line := unmarshal[string](t, c[0])
		if got, want := jsonArrayHead(line), unmarshal[bool](t, c[1]); got != want {
			t.Errorf("jsonArrayHead(%q) = %v, Node %v", line, got, want)
		}
	}
	for _, c := range g.Names {
		frag, text := unmarshal[string](t, c[0]), unmarshal[string](t, c[1])
		if got, want := newNameMatcher(frag).test(text), unmarshal[bool](t, c[2]); got != want {
			t.Errorf("nameMatcher(%q).test(%q) = %v, Node %v", frag, text, got, want)
		}
	}
	for _, c := range g.Preambles {
		text := unmarshal[string](t, c[0])
		want := unmarshal[*string](t, c[1])
		got := detectReasoningPreamble(text)
		if (want == nil) != (got == "") || want != nil && *want != got {
			t.Errorf("detectReasoningPreamble(%.30q) = %q, Node %v", text, got, c[1])
		}
	}
	for _, c := range g.Rebuild {
		soul, minChars := unmarshal[string](t, c[0]), unmarshal[float64](t, c[1])
		if got, want := soulNeedsRebuild(soul, minChars), unmarshal[bool](t, c[2]); got != want {
			t.Errorf("soulNeedsRebuild(%.20q, %v) = %v, Node %v", soul, minChars, got, want)
		}
	}
}

// prefixOf reads a golden prefix value: {"undefined":true} is absent, a
// string is itself, anything else is not a string.
func prefixOf(raw json.RawMessage) (p scopePrefix, isString bool) {
	var compact bytes.Buffer
	if json.Compact(&compact, raw) == nil {
		raw = compact.Bytes()
	}
	if string(raw) == `{"undefined":true}` || string(raw) == "null" {
		return scopePrefix{}, false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return scopePrefix{value: s}, true
	}
	return scopePrefix{notString: true}, false
}

func TestScopeHelpersMatchNode(t *testing.T) {
	g := loadGoldens(t)
	for _, c := range g.Prefixes {
		p, isString := prefixOf(c.In)
		got, err := resolveScopePrefix(p)
		if c.Resolved.Error != "" {
			if err == nil || err.Error() != c.Resolved.Error {
				t.Errorf("resolveScopePrefix(%s) = %q, %v; Node threw %q", c.In, got, err, c.Resolved.Error)
			}
		} else if err != nil || c.Resolved.Value == nil || got != *c.Resolved.Value {
			t.Errorf("resolveScopePrefix(%s) = %q, %v; Node %v", c.In, got, err, c.Resolved.Value)
		}
		// Roster prefixes come from a text column: a string or NULL.
		if isString || strings.TrimSpace(string(c.In)) == "null" {
			var row *string
			if isString {
				row = &p.value
			}
			got := validateRosterPrefix(row)
			if (c.Roster == nil) != (got == "") || c.Roster != nil && *c.Roster != got {
				t.Errorf("validateRosterPrefix(%s) = %q, Node %v", c.In, got, c.Roster)
			}
		} else if c.Roster != nil {
			t.Errorf("validateRosterPrefix(%s): Node accepted a non-string", c.In)
		}
	}
	for _, c := range g.PeoplePaths {
		p, _ := prefixOf(c.Prefix)
		got, err := peopleNotePath(p, c.Slug)
		if c.Path.Error != "" {
			if err == nil || err.Error() != c.Path.Error {
				t.Errorf("peopleNotePath(%s, %q) = %q, %v; Node threw %q", c.Prefix, c.Slug, got, err, c.Path.Error)
			}
		} else if err != nil || got != *c.Path.Value {
			t.Errorf("peopleNotePath(%s, %q) = %q, %v; Node %q", c.Prefix, c.Slug, got, err, *c.Path.Value)
		}
		valid := validatePersonSlug(c.Slug)
		if (c.Valid == nil) != (valid == "") || c.Valid != nil && *c.Valid != valid {
			t.Errorf("validatePersonSlug(%q) = %q, Node %v", c.Slug, valid, c.Valid)
		}
	}
	for _, c := range g.Slugs {
		got := peopleslug.Slug(*c[0])
		if (c[1] == nil) != (got == "") || c[1] != nil && *c[1] != got {
			t.Errorf("personContextSlug(%q) = %q, Node %v", *c[0], got, c[1])
		}
	}
	for _, c := range g.SlugPrefixes {
		if got := sim.NormalizeSlugPrefix(c[0]); got != c[1] {
			t.Errorf("normalizeSlugPrefix(%.30q) = %q, Node %q", c[0], got, c[1])
		}
		if got := sim.CollapseTrailingSlashes(c[0]); got != c[2] {
			t.Errorf("collapseTrailingSlashes(%.30q) = %q, Node %q", c[0], got, c[2])
		}
	}
	for _, c := range g.Displays {
		if got := sim.SlugToDisplay(c[0]); got != c[1] {
			t.Errorf("slugToDisplay(%q) = %q, Node %q", c[0], got, c[1])
		}
	}
	for _, c := range g.Delays {
		store := config.NewStore()
		if c.Value != nil {
			store.Set("k", *c.Value)
		}
		j := &Job{Config: store}
		got, err := j.dreamDelayMs("k", c.Fallback)
		if c.Result.Error != "" {
			if err == nil || err.Error() != c.Result.Error {
				t.Errorf("dreamDelayMs(%v) = %v, %v; Node threw %q", c.Value, got, err, c.Result.Error)
			}
		} else if err != nil || got != *c.Result.Value {
			t.Errorf("dreamDelayMs(%v) = %v, %v; Node %v", c.Value, got, err, *c.Result.Value)
		}
	}
}

// ---------------------------------------------------------------- passes

// stream records the run's events in order, in the shape goldens.js
// writes them.
type stream struct {
	mu     sync.Mutex
	events []map[string]any
}

func (s *stream) add(e map[string]any) {
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
}

// scriptDB answers statements from the scenario's script in order and
// refuses one that does not contain the scripted match.
type scriptDB struct {
	s      *stream
	script []scriptStep
	conn   *scriptConn
}

func fold(sql string) string { return strings.Join(strings.Fields(sql), " ") }

func param(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return x
	case *string:
		if x == nil {
			return nil
		}
		return *x
	case int:
		return strconv.Itoa(x)
	case time.Time:
		// prepareValue's dateToString with TZ=UTC.
		return x.UTC().Format("2006-01-02T15:04:05.000+00:00")
	}
	return fmt.Sprintf("unexpected %T", v)
}

func (d *scriptDB) next(sql string, args []any, onConn bool) (scriptStep, error) {
	params := make([]any, len(args))
	for i, a := range args {
		params[i] = param(a)
	}
	d.s.add(map[string]any{"kind": "query", "sql": fold(sql), "params": params, "conn": onConn})
	if len(d.script) == 0 {
		return scriptStep{}, fmt.Errorf("unscripted statement: %.60s", strings.TrimSpace(sql))
	}
	step := d.script[0]
	d.script = d.script[1:]
	if !strings.Contains(sql, step.Match) {
		return scriptStep{}, fmt.Errorf("statement does not match %q", step.Match)
	}
	if step.Lose != "" && d.conn != nil {
		d.conn.lost = errors.New(step.Lose)
	}
	if step.Error != "" {
		return scriptStep{}, &pgconn.PgError{Severity: "ERROR", Message: step.Error}
	}
	return step, nil
}

func (d *scriptDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	_, err := d.next(sql, args, false)
	return pgconn.NewCommandTag("UPDATE 1"), err
}

func (d *scriptDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	step, err := d.next(sql, args, false)
	if err != nil {
		return nil, err
	}
	return &scriptRows{rows: step.Rows, i: -1}, nil
}

func (d *scriptDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return d.row(sql, args, false)
}

func (d *scriptDB) row(sql string, args []any, onConn bool) pgx.Row {
	step, err := d.next(sql, args, onConn)
	if err != nil {
		return errRow{err}
	}
	if len(step.Rows) == 0 {
		return errRow{pgx.ErrNoRows}
	}
	return &scriptRows{rows: step.Rows[:1], i: 0}
}

func (d *scriptDB) Begin(context.Context) (pgx.Tx, error) { return nil, errors.New("no transactions") }

type scriptConn struct {
	db   *scriptDB
	lost error
}

func (c *scriptConn) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return c.db.row(sql, args, true)
}

func (c *scriptConn) Ping(context.Context) error { return c.lost }

func (c *scriptConn) Release(destroy bool) {
	c.db.s.add(map[string]any{"kind": "release", "destroy": destroy})
}

type scriptConnector struct {
	db  *scriptDB
	err string
}

func (c *scriptConnector) Connect(context.Context) (Conn, error) {
	c.db.s.add(map[string]any{"kind": "connect"})
	if c.err != "" {
		return nil, errors.New(c.err)
	}
	c.db.conn = &scriptConn{db: c.db}
	return c.db.conn, nil
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

type scriptRows struct {
	pgx.Rows
	rows [][]any
	i    int
}

func (r *scriptRows) Next() bool { r.i++; return r.i < len(r.rows) }
func (r *scriptRows) Close()     {}
func (r *scriptRows) Err() error { return nil }

func (r *scriptRows) Scan(dest ...any) error {
	row := r.rows[r.i]
	if len(dest) != len(row) {
		return fmt.Errorf("scan %d values into %d destinations", len(row), len(dest))
	}
	for i, v := range row {
		if err := assign(dest[i], v); err != nil {
			return fmt.Errorf("column %d: %w", i, err)
		}
	}
	return nil
}

func parsePG(v any) (time.Time, error) {
	return time.Parse("2006-01-02 15:04:05.999999999-07", v.(string))
}

func assign(dest, v any) error {
	switch d := dest.(type) {
	case *int:
		*d = int(v.(float64))
	case **int:
		if v == nil {
			*d = nil
		} else {
			n := int(v.(float64))
			*d = &n
		}
	case *string:
		*d = v.(string)
	case **string:
		if v == nil {
			*d = nil
		} else {
			s := v.(string)
			*d = &s
		}
	case **bool:
		if v == nil {
			*d = nil
		} else {
			b := v.(bool)
			*d = &b
		}
	case *time.Time:
		ts, err := parsePG(v)
		if err != nil {
			return err
		}
		*d = ts
	case **time.Time:
		if v == nil {
			*d = nil
			return nil
		}
		ts, err := parsePG(v)
		if err != nil {
			return err
		}
		*d = &ts
	default:
		return fmt.Errorf("unsupported destination %T", dest)
	}
	return nil
}

type fakeLog struct{ s *stream }

func (l fakeLog) Log(subsystem, action string, details logger.Fields) {
	b, err := jsonutil.Marshal(details)
	if err != nil {
		b = []byte("marshal error: " + err.Error())
	}
	l.s.add(map[string]any{"kind": "log", "subsystem": subsystem, "action": action, "details": string(b)})
}

func (l fakeLog) LogError(subsystem, action string, opts logger.ErrorOpts) {
	l.s.add(map[string]any{"kind": "error", "subsystem": subsystem, "action": action, "agent": opts.Agent, "context": opts.Context, "message": opts.Message})
}

func (fakeLog) Errorf(string, ...any) {}

type fakeNotes struct {
	s      *stream
	in     map[string]string
	lists  map[string][]string
	fail   []*string
	nSaves int
}

func (n *fakeNotes) Save(_ context.Context, in notes.SaveInput) (notes.SavedNote, error) {
	n.s.add(map[string]any{"kind": "save", "namespace": in.Namespace, "title": in.Title, "content": in.Content, "slug": in.Slug, "created_by": in.CreatedBy, "upsert": in.Upsert})
	n.nSaves++
	if n.nSaves <= len(n.fail) && n.fail[n.nSaves-1] != nil {
		return notes.SavedNote{}, errors.New(*n.fail[n.nSaves-1])
	}
	return notes.SavedNote{ID: 1}, nil
}

func (n *fakeNotes) Read(_ context.Context, namespace, slug string) (notes.Note, error) {
	n.s.add(map[string]any{"kind": "read", "namespace": namespace, "slug": slug})
	content, ok := n.in[slug]
	if !ok {
		return notes.Note{}, errors.New("Note not found: " + slug)
	}
	return notes.Note{Slug: slug, Content: content}, nil
}

func (n *fakeNotes) ListLive(_ context.Context, in notes.ListInput) ([]notes.ListedNote, error) {
	n.s.add(map[string]any{"kind": "list", "namespace": in.Namespace, "limit": string(in.Limit), "offset": string(in.Offset), "prefix": in.Prefix})
	var out []notes.ListedNote
	for _, slug := range n.lists[in.Prefix] {
		out = append(out, notes.ListedNote{Slug: slug})
	}
	return out, nil
}

type fakeAgents struct {
	t       *testing.T
	s       *stream
	answers []answer
}

func (a *fakeAgents) Invoke(_ context.Context, agent string, opts virtualagent.InvokeOptions) (*virtualagent.InvokeResult, error) {
	a.s.add(map[string]any{
		"kind": "invoke", "agent": agent, "user": opts.UserMessage, "context": opts.Context,
		"skipRateLimit": opts.SkipRateLimit, "skipCostLimit": opts.SkipCostLimit, "skipRetry": !opts.Retry,
	})
	if len(a.answers) == 0 {
		return nil, errors.New("no answer scripted")
	}
	ans := a.answers[0]
	a.answers = a.answers[1:]
	if ans.Error != "" {
		return nil, errors.New(ans.Error)
	}
	var text any = "reply"
	if ans.Text != nil {
		v, err := jsutil.Parse(ans.Text)
		if err != nil {
			a.t.Fatal(err)
		}
		text = v
	}
	usage := providers.Usage{InputTokens: json.Number("10"), OutputTokens: json.Number("5")}
	if ans.Usage != nil {
		usage = providers.Usage{}
		if v, err := jsutil.Parse(ans.Usage); err == nil {
			if o, ok := v.(*jsutil.Object); ok {
				if x, ok := o.Get("input_tokens"); ok {
					usage.InputTokens = x
				}
				if x, ok := o.Get("output_tokens"); ok {
					usage.OutputTokens = x
				}
			}
		}
	}
	finish := ans.Finish
	if finish == "" {
		finish = "stop"
	}
	return &virtualagent.InvokeResult{Text: text, Usage: usage, FinishReason: finish, Truncated: ans.Truncated}, nil
}

type fakeTrust struct {
	s       *stream
	trusted []int
	err     string
}

func (f fakeTrust) IsTrustedCreator(_ context.Context, id int) (bool, error) {
	f.s.add(map[string]any{"kind": "trust", "id": id})
	if f.err != "" {
		return false, errors.New(f.err)
	}
	for _, x := range f.trusted {
		if x == id {
			return true, nil
		}
	}
	return false, nil
}

func storeOf(values map[string]string) *config.Store {
	store := config.NewStore()
	for k, v := range values {
		store.Set(k, v)
	}
	return store
}

// newJob builds a Job over the golden's fakes.
func newJob(t *testing.T, p passGolden, s *stream) (*Job, *scriptDB, *fakeAgents) {
	clock, err := time.Parse(time.RFC3339Nano, p.Clock)
	if err != nil {
		t.Fatal(err)
	}
	db := &scriptDB{s: s, script: append([]scriptStep(nil), p.Script...)}
	agents := &fakeAgents{t: t, s: s, answers: append([]answer(nil), p.Answers...)}
	return &Job{
		DB:     db,
		Conns:  &scriptConnector{db: db, err: p.ConnectError},
		Config: storeOf(p.Config),
		Log:    fakeLog{s},
		Notes:  &fakeNotes{s: s, in: p.NotesIn, lists: p.Lists, fail: p.FailNth["save"]},
		Agents: agents,
		Trust:  fakeTrust{s: s, trusted: p.Trusted, err: p.TrustError},
		Now:    func() time.Time { return clock },
		Sleep: func(_ context.Context, ms float64) error {
			s.add(map[string]any{"kind": "sleep", "ms": ms})
			return nil
		},
	}, db, agents
}

// normalized is the event as JSON values, with Node's statement text
// folded like Go's.
func normalized(t *testing.T, e map[string]any) any {
	if e["kind"] == "query" {
		e = cloneMap(e)
		e["sql"] = fold(e["sql"].(string))
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	_ = json.Unmarshal(b, &v)
	return v
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// isLockLossLog: Node logs a lost lock when the client's error event
// fires, Go at the first check after it (Known differences), so those lines
// are compared apart from the order.
func isLockLossLog(e map[string]any) bool {
	return e["kind"] == "log" && e["action"] == "lock-connection-error"
}

func split(events []map[string]any) (ordered, loss []map[string]any) {
	for _, e := range events {
		if isLockLossLog(e) {
			loss = append(loss, e)
			continue
		}
		ordered = append(ordered, e)
	}
	return ordered, loss
}

func compareEvents(t *testing.T, name string, got, want []map[string]any) {
	t.Helper()
	gotOrdered, gotLoss := split(got)
	wantOrdered, wantLoss := split(want)
	n := max(len(gotOrdered), len(wantOrdered))
	for i := 0; i < n; i++ {
		var g, w any
		if i < len(gotOrdered) {
			g = normalized(t, gotOrdered[i])
		}
		if i < len(wantOrdered) {
			w = normalized(t, wantOrdered[i])
		}
		if !reflect.DeepEqual(g, w) {
			gb, _ := json.Marshal(g)
			wb, _ := json.Marshal(w)
			t.Errorf("%s: event %d\n got %s\nwant %s", name, i, gb, wb)
			return
		}
	}
	if !sameJSON(t, gotLoss, wantLoss) {
		t.Errorf("%s: lock-loss lines %v, Node %v", name, gotLoss, wantLoss)
	}
}

func TestPassesMatchNode(t *testing.T) {
	g := loadGoldens(t)
	if len(g.Passes) == 0 {
		t.Fatal("no pass scenarios")
	}
	for _, p := range g.Passes {
		if p.Unused != 0 || p.UnusedAnswers != 0 {
			t.Errorf("%s: the golden left %d statements and %d answers unused", p.Name, p.Unused, p.UnusedAnswers)
		}
		s := &stream{}
		j, db, agents := newJob(t, p, s)
		j.Pass(context.Background(), "0 4 * * *")
		if len(db.script) != p.Unused || len(agents.answers) != p.UnusedAnswers {
			t.Errorf("%s: %d statements and %d answers unused, Node left %d and %d", p.Name, len(db.script), len(agents.answers), p.Unused, p.UnusedAnswers)
		}
		compareEvents(t, p.Name, s.events, p.Events)
	}
}

func TestStartMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Starts {
		s := &stream{}
		cfg := map[string]string{}
		if c.Schedule != nil {
			cfg["dream_cron_schedule"] = *c.Schedule
		}
		j := &Job{Config: storeOf(cfg), Log: fakeLog{s}}
		task, err := j.Start()
		threw := ""
		if err != nil {
			threw = err.Error()
		}
		if task != nil {
			task.Stop()
		}
		if (task != nil) != c.Started || threw != c.Threw {
			t.Errorf("schedule %v: started %v, threw %q; Node %v, %q", c.Schedule, task != nil, threw, c.Started, c.Threw)
		}
		compareEvents(t, fmt.Sprintf("schedule %v", c.Schedule), s.events, c.Events)
	}
}
