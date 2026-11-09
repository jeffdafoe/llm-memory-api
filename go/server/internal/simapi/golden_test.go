package simapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/sim"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/virtualagent"
)

// goldens is testdata/goldens.json, recorded from
// Node's sim-conversation-distiller.js, sim-soul.js and routes/sim.js.
type goldens struct {
	Narrate []struct {
		Event json.RawMessage `json:"event"`
		Text  *string         `json:"text"`
		Error string          `json:"error"`
	} `json:"narrate"`
	SoulMessages []struct {
		In      map[string]json.RawMessage `json:"in"`
		Message string                     `json:"message"`
	} `json:"soulMessages"`
	Distills []scenario `json:"distills"`
	Souls    []scenario `json:"souls"`
	Raws     []scenario `json:"raws"`
}

type scenario struct {
	Name      string                     `json:"name"`
	Input     map[string]json.RawMessage `json:"input"`
	Body      json.RawMessage            `json:"body"`
	Script    []scriptStep               `json:"script"`
	Answers   []answer                   `json:"answers"`
	SaveError string                     `json:"saveError"`
	Unused    int                        `json:"unused"`
	Events    []map[string]any           `json:"events"`
	Outcome   struct {
		Value  *string `json:"value"`
		Error  string  `json:"error"`
		Status int     `json:"status"`
	} `json:"outcome"`
}

type answer struct {
	Text      json.RawMessage `json:"text"`
	Truncated bool            `json:"truncated"`
	Finish    string          `json:"finish"`
	Error     string          `json:"error"`
}

type scriptStep struct {
	Match   string   `json:"match"`
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	Error   string   `json:"error"`
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

func parse(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	if raw == nil {
		return jsutil.Undefined
	}
	v, err := jsutil.Parse(raw)
	if err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return v
}

func TestNarrateMatchesNode(t *testing.T) {
	unmapped := &sim.UnmappedKinds{}
	for _, c := range loadGoldens(t).Narrate {
		event := parse(t, c.Event).(*jsutil.Object)
		got, err := sim.NarrateEvent(event, unmapped)
		switch {
		case c.Error != "":
			if err == nil || err.Error() != c.Error {
				t.Errorf("%s: %q, %v; Node threw %q", c.Event, got, err, c.Error)
			}
		case err != nil:
			t.Errorf("%s: %v", c.Event, err)
		case c.Text == nil && got != "", c.Text != nil && got != *c.Text:
			t.Errorf("%s:\n got %q\nwant %v", c.Event, got, c.Text)
		}
	}
}

func TestSoulMessageMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).SoulMessages {
		str := func(k string) string {
			s, _ := parse(t, c.In[k]).(string)
			return s
		}
		got := sim.BuildSoulUserMessage(sim.SoulMessage{
			CharacterDescription: str("characterDescription"), CurrentSoul: str("currentSoul"),
			DaySnapshot: str("daySnapshot"), Day: str("day"),
		})
		if got != c.Message {
			t.Errorf("soul message:\n got %q\nwant %q", got, c.Message)
		}
	}
}

// ------------------------------------------------------------------ fakes

type stream struct{ events []map[string]any }

func (s *stream) add(e map[string]any) { s.events = append(s.events, e) }

func fold(sql string) string { return strings.Join(strings.Fields(sql), " ") }

type scriptDB struct {
	s      *stream
	script []scriptStep
}

func param(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case string:
		return x
	case int:
		return strconv.Itoa(x)
	}
	return fmt.Sprintf("unexpected %T", v)
}

// textCasts are the columns Go reads as text in SQL, where node-postgres
// returned bigint, numeric and uuid as strings itself.
var textCasts = strings.NewReplacer("c.id::text", "c.id", "c.scene_id::text", "c.scene_id", "c.cost::text", "c.cost")

func (d *scriptDB) next(sql string, args []any) (scriptStep, error) {
	params := make([]any, len(args))
	for i, a := range args {
		params[i] = param(a)
	}
	d.s.add(map[string]any{"kind": "query", "sql": fold(textCasts.Replace(sql)), "params": params})
	if len(d.script) == 0 {
		return scriptStep{}, fmt.Errorf("unscripted statement: %.60s", strings.TrimSpace(sql))
	}
	step := d.script[0]
	d.script = d.script[1:]
	if !strings.Contains(sql, step.Match) {
		return scriptStep{}, fmt.Errorf("statement does not match %q", step.Match)
	}
	if step.Error != "" {
		return scriptStep{}, &pgconn.PgError{Message: step.Error}
	}
	return step, nil
}

func (d *scriptDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	_, err := d.next(sql, args)
	return pgconn.NewCommandTag("INSERT 0 1"), err
}

func (d *scriptDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	step, err := d.next(sql, args)
	if err != nil {
		return nil, err
	}
	return &rows{rows: step.Rows, i: -1}, nil
}

func (d *scriptDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	step, err := d.next(sql, args)
	if err != nil {
		return errRow{err}
	}
	if len(step.Rows) == 0 {
		return errRow{pgx.ErrNoRows}
	}
	return &rows{rows: step.Rows[:1], i: 0}
}

func (d *scriptDB) Begin(context.Context) (pgx.Tx, error) { return nil, errors.New("no transactions") }

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

type rows struct {
	pgx.Rows
	rows [][]any
	i    int
}

func (r *rows) Next() bool { r.i++; return r.i < len(r.rows) }
func (r *rows) Close()     {}
func (r *rows) Err() error { return nil }

func (r *rows) Scan(dest ...any) error {
	row := r.rows[r.i]
	if len(dest) != len(row) {
		return fmt.Errorf("scan %d values into %d destinations", len(row), len(dest))
	}
	for i, v := range row {
		switch d := dest[i].(type) {
		case *int:
			*d = int(v.(float64))
		case **int:
			if v != nil {
				n := int(v.(float64))
				*d = &n
			}
		case *string:
			*d = v.(string)
		case **string:
			if v != nil {
				s := v.(string)
				*d = &s
			}
		case **bool:
			if v != nil {
				b := v.(bool)
				*d = &b
			}
		case *time.Time:
			ts, err := time.Parse("2006-01-02 15:04:05.999999999-07", v.(string))
			if err != nil {
				return err
			}
			*d = ts
		default:
			return fmt.Errorf("unsupported destination %T", dest[i])
		}
	}
	return nil
}

type fakeLog struct{ s *stream }

func (l fakeLog) Log(subsystem, action string, details logger.Fields) {
	b, _ := jsonutil.Marshal(details)
	l.s.add(map[string]any{"kind": "log", "subsystem": subsystem, "action": action, "details": string(b)})
}

func (l fakeLog) LogError(subsystem, action string, opts logger.ErrorOpts) {
	l.s.add(map[string]any{"kind": "error", "subsystem": subsystem, "action": action, "agent": opts.Agent, "message": opts.Message, "status": opts.StatusCode})
}

func (fakeLog) Errorf(string, ...any) {}

type fakeNotes struct {
	s   *stream
	err string
}

func (n fakeNotes) Save(_ context.Context, in notes.SaveInput) (notes.SavedNote, error) {
	n.s.add(map[string]any{"kind": "save", "namespace": in.Namespace, "title": in.Title, "content": in.Content, "slug": in.Slug, "created_by": in.CreatedBy, "upsert": in.Upsert})
	if n.err != "" {
		return notes.SavedNote{}, errors.New(n.err)
	}
	return notes.SavedNote{ID: 1}, nil
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
		text = parse(a.t, ans.Text)
	}
	finish := ans.Finish
	if finish == "" {
		finish = "stop"
	}
	return &virtualagent.InvokeResult{Text: text, FinishReason: finish, Truncated: ans.Truncated}, nil
}

type fakeTrust struct{ s *stream }

func (f fakeTrust) IsTrustedCreator(_ context.Context, id int) (bool, error) {
	f.s.add(map[string]any{"kind": "trust", "id": id})
	return id == 1, nil
}

func newService(t *testing.T, sc scenario, s *stream) (*Service, *scriptDB) {
	db := &scriptDB{s: s, script: append([]scriptStep(nil), sc.Script...)}
	return &Service{
		DB: db, Log: fakeLog{s}, Notes: fakeNotes{s: s, err: sc.SaveError},
		Agents: &fakeAgents{t: t, s: s, answers: append([]answer(nil), sc.Answers...)}, Trust: fakeTrust{s},
	}, db
}

// normalize folds Node's statement text and turns Node's sim-distill log
// lines into Go's shape: Node passed the details object as the action
// (logSim), so it printed [object Object]; Go logs the event as the action
// (Known differences).
func normalize(e map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range e {
		out[k] = v
	}
	if out["kind"] == "query" {
		out["sql"] = fold(out["sql"].(string))
	}
	return out
}

func distillLogOf(e map[string]any) (map[string]any, bool) {
	if e["kind"] != "log" || e["subsystem"] != "sim-distill" {
		return nil, false
	}
	return e, true
}

func sameEvents(t *testing.T, name string, got, want []map[string]any) {
	t.Helper()
	g, _ := json.Marshal(eventsJSON(got, false))
	w, _ := json.Marshal(eventsJSON(want, true))
	if !bytes.Equal(g, w) {
		t.Errorf("%s: events\n got %s\nwant %s", name, g, w)
	}
}

// eventsJSON is the comparable form. A sim-distill log becomes its fields
// with the action as "event" on both sides.
func eventsJSON(events []map[string]any, node bool) []any {
	var out []any
	for _, e := range events {
		if l, ok := distillLogOf(e); ok {
			var fields map[string]any
			if node {
				_ = json.Unmarshal([]byte(l["action"].(string)), &fields)
			} else {
				_ = json.Unmarshal([]byte(l["details"].(string)), &fields)
				fields["event"] = l["action"]
			}
			out = append(out, map[string]any{"sim-distill": fields})
			continue
		}
		b, _ := json.Marshal(normalize(e))
		var v any
		_ = json.Unmarshal(b, &v)
		out = append(out, v)
	}
	return out
}

func checkOutcome(t *testing.T, sc scenario, value any, err error) {
	t.Helper()
	if sc.Outcome.Value != nil {
		if err != nil {
			t.Errorf("%s: error %v, Node returned %s", sc.Name, err, *sc.Outcome.Value)
			return
		}
		b, _ := jsonutil.Marshal(value)
		if string(b) != *sc.Outcome.Value {
			t.Errorf("%s:\n got %s\nwant %s", sc.Name, b, *sc.Outcome.Value)
		}
		return
	}
	status := 0
	var apiErr *apierr.Error
	if errors.As(err, &apiErr) {
		status = apiErr.Status
	}
	if err == nil || message(err) != sc.Outcome.Error || status != sc.Outcome.Status {
		t.Errorf("%s: %v (status %d), Node threw %q (status %d)", sc.Name, err, status, sc.Outcome.Error, sc.Outcome.Status)
	}
}

func message(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Message
	}
	return err.Error()
}

func TestDistillMatchesNode(t *testing.T) {
	for _, sc := range loadGoldens(t).Distills {
		s := &stream{}
		svc, db := newService(t, sc, s)
		in := DistillInput{
			Day: parse(t, sc.Input["day"]), Events: parse(t, sc.Input["events"]),
			SlugPrefix: parse(t, sc.Input["slugPrefix"]), Actor: parse(t, sc.Input["actorName"]),
		}
		if agent, ok := parse(t, sc.Input["agent"]).(string); ok {
			in.Agent, in.AgentIsString = agent, true
		}
		value, err := svc.Distill(context.Background(), in)
		checkOutcome(t, sc, value, err)
		if len(db.script) != sc.Unused {
			t.Errorf("%s: %d statements unused, Node %d", sc.Name, len(db.script), sc.Unused)
		}
		sameEvents(t, sc.Name, s.events, sc.Events)
	}
}

func TestSoulMatchesNode(t *testing.T) {
	for _, sc := range loadGoldens(t).Souls {
		s := &stream{}
		svc, db := newService(t, sc, s)
		value, err := svc.SynthesizeSoul(context.Background(), SoulInput{
			CharacterDescription: parse(t, sc.Input["characterDescription"]),
			CurrentSoul:          parse(t, sc.Input["currentSoul"]),
			DaySnapshot:          parse(t, sc.Input["daySnapshot"]),
			Day:                  parse(t, sc.Input["day"]),
		})
		checkOutcome(t, sc, value, err)
		if len(db.script) != sc.Unused {
			t.Errorf("%s: %d statements unused, Node %d", sc.Name, len(db.script), sc.Unused)
		}
		sameEvents(t, sc.Name, s.events, sc.Events)
	}
}

// route is Router.Route's answer for a handler error (the wrapper is
// tested with the routes elsewhere).
func route(fn httpapi.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			status, code := apierr.StatusAndCode(err)
			if status < 500 {
				httpapi.WriteError(w, status, code, err.Error())
				return
			}
			httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL", "Failed: raw_turns")
		}
	}
}

func TestRawTurnsMatchNode(t *testing.T) {
	for _, sc := range loadGoldens(t).Raws {
		s := &stream{}
		svc, db := newService(t, sc, s)
		rt := &Routes{Service: svc}
		req := httptest.NewRequest(http.MethodPost, "/v1/sim/raw-turns", nil)
		rec := httptest.NewRecorder()
		httpapi.WithState(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if sc.Body != nil {
				httpapi.StateOf(r).Body = sc.Body
			}
			route(rt.rawTurns)(w, r)
		})).ServeHTTP(rec, req)

		var want struct {
			Status int    `json:"status"`
			Body   string `json:"body"`
		}
		if err := json.Unmarshal([]byte(*sc.Outcome.Value), &want); err != nil {
			t.Fatal(err)
		}
		var gotBody, wantBody any
		_ = json.Unmarshal(rec.Body.Bytes(), &gotBody)
		_ = json.Unmarshal([]byte(want.Body), &wantBody)
		if rec.Code != want.Status || !reflect.DeepEqual(gotBody, wantBody) {
			t.Errorf("%s: %d %s\nNode %d %s", sc.Name, rec.Code, rec.Body.String(), want.Status, want.Body)
		}
		// The body as written, key order included.
		if rec.Code == http.StatusOK && strings.TrimSpace(rec.Body.String()) != want.Body {
			t.Errorf("%s: body text\n got %s\nwant %s", sc.Name, rec.Body.String(), want.Body)
		}
		if len(db.script) != sc.Unused {
			t.Errorf("%s: %d statements unused, Node %d", sc.Name, len(db.script), sc.Unused)
		}
		// error_log rows are the route wrapper's, not compared here.
		var gotQ, wantQ []map[string]any
		for _, e := range s.events {
			if e["kind"] == "query" {
				gotQ = append(gotQ, e)
			}
		}
		for _, e := range sc.Events {
			if e["kind"] == "query" {
				wantQ = append(wantQ, e)
			}
		}
		sameEvents(t, sc.Name, gotQ, wantQ)
	}
}
