package cleanup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
)

// goldens is testdata/goldens.json, written by testdata/goldens.js from
// Node's cleanup.js with its database scripted. Regenerate it with that
// script when the Node side changes.
type goldens struct {
	Pass []struct {
		Name    string            `json:"name"`
		Config  map[string]string `json:"config"`
		Script  []scriptStep      `json:"script"`
		Unused  int               `json:"unused"`
		Queries []query           `json:"queries"`
		Logs    [][3]string       `json:"logs"`
		Errors  [][3]string       `json:"errors"`
	} `json:"pass"`
	Start []struct {
		Schedule *string     `json:"schedule"`
		Started  bool        `json:"started"`
		Threw    string      `json:"threw"`
		Logs     [][3]string `json:"logs"`
	} `json:"start"`
}

type scriptStep struct {
	Match    string   `json:"match"`
	Columns  []string `json:"columns"`
	Rows     [][]any  `json:"rows"`
	RowCount *int64   `json:"rowCount"`
	Error    string   `json:"error"`
}

type query struct {
	SQL    string    `json:"sql"`
	Params []*string `json:"params"`
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

// scriptDB answers statements from a script in order, as the Node golden's
// stub pool did, and records each statement and the transaction marks.
type scriptDB struct {
	mu      sync.Mutex
	script  []scriptStep
	queries []query
	marks   []string // statement first words and BEGIN / COMMIT / ROLLBACK
}

func fold(sql string) string { return strings.Join(strings.Fields(sql), " ") }

func param(v any) *string {
	var s string
	switch x := v.(type) {
	case nil:
		return nil
	case jsutil.Number:
		s = string(x)
	case string:
		s = x
	case int:
		s = strconv.Itoa(x)
	case int64:
		s = strconv.FormatInt(x, 10)
	default:
		s = fmt.Sprintf("unexpected %T", v)
	}
	return &s
}

func (d *scriptDB) next(sql string, args []any) (scriptStep, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := query{SQL: sql}
	for _, a := range args {
		q.Params = append(q.Params, param(a))
	}
	d.queries = append(d.queries, q)
	d.marks = append(d.marks, strings.Fields(sql)[0])
	if len(d.script) == 0 {
		return scriptStep{}, fmt.Errorf("unscripted statement: %.60s", strings.TrimSpace(sql))
	}
	step := d.script[0]
	d.script = d.script[1:]
	if !strings.Contains(sql, step.Match) {
		return scriptStep{}, fmt.Errorf("statement does not match %q: %.60s", step.Match, strings.TrimSpace(sql))
	}
	if step.Error != "" {
		return scriptStep{}, &pgconn.PgError{Severity: "ERROR", Message: step.Error}
	}
	return step, nil
}

func (d *scriptDB) mark(m string) {
	d.mu.Lock()
	d.marks = append(d.marks, m)
	d.mu.Unlock()
}

func (s scriptStep) tag() pgconn.CommandTag {
	n := int64(len(s.Rows))
	if s.RowCount != nil {
		n = *s.RowCount
	}
	return pgconn.NewCommandTag(fmt.Sprintf("X %d", n))
}

func (d *scriptDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	step, err := d.next(sql, args)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	return step.tag(), nil
}

func (d *scriptDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	step, err := d.next(sql, args)
	if err != nil {
		return nil, err
	}
	return &scriptRows{rows: step.Rows, i: -1}, nil
}

func (d *scriptDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	step, err := d.next(sql, args)
	if err != nil {
		return errRow{err}
	}
	if len(step.Rows) == 0 {
		return errRow{pgx.ErrNoRows}
	}
	return &scriptRows{rows: step.Rows[:1], i: 0}
}

func (d *scriptDB) Begin(context.Context) (pgx.Tx, error) {
	d.mark("BEGIN")
	return &scriptTx{db: d}, nil
}

type scriptTx struct {
	pgx.Tx
	db   *scriptDB
	done bool
}

func (t *scriptTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.Exec(ctx, sql, args...)
}

func (t *scriptTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.db.Query(ctx, sql, args...)
}

func (t *scriptTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.db.QueryRow(ctx, sql, args...)
}

func (t *scriptTx) Commit(context.Context) error {
	t.done = true
	t.db.mark("COMMIT")
	return nil
}

func (t *scriptTx) Rollback(context.Context) error {
	if t.done {
		return pgx.ErrTxClosed
	}
	t.done = true
	t.db.mark("ROLLBACK")
	return nil
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

type scriptRows struct {
	pgx.Rows
	rows [][]any
	i    int
}

func (r *scriptRows) Next() bool                    { r.i++; return r.i < len(r.rows) }
func (r *scriptRows) Close()                        {}
func (r *scriptRows) Err() error                    { return nil }
func (r *scriptRows) CommandTag() pgconn.CommandTag { return pgconn.NewCommandTag("X 0") }

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

func assign(dest, v any) error {
	switch d := dest.(type) {
	case *int64:
		*d = int64(v.(float64))
	case **int64:
		if v == nil {
			*d = nil
		} else {
			n := int64(v.(float64))
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
	case **time.Time:
		if v == nil {
			*d = nil
		} else {
			ts, err := time.Parse(time.RFC3339Nano, v.(string))
			if err != nil {
				return err
			}
			*d = &ts
		}
	default:
		return fmt.Errorf("unsupported destination %T", dest)
	}
	return nil
}

// recorder captures the job's log lines and error_log calls.
type recorder struct {
	mu      sync.Mutex
	logs    [][3]string
	errors  [][3]string
	details []string
}

func (r *recorder) Log(subsystem, action string, details logger.Fields) {
	b, err := jsonutil.Marshal(details)
	if err != nil {
		b = []byte("marshal error: " + err.Error())
	}
	r.mu.Lock()
	r.logs = append(r.logs, [3]string{subsystem, action, string(b)})
	r.mu.Unlock()
}

func (r *recorder) LogError(subsystem, action string, opts logger.ErrorOpts) {
	r.mu.Lock()
	r.errors = append(r.errors, [3]string{subsystem, action, opts.Message})
	r.details = append(r.details, opts.Detail)
	r.mu.Unlock()
}

func (r *recorder) Errorf(string, ...any) {}

func storeOf(values map[string]string) *config.Store {
	store := config.NewStore()
	for k, v := range values {
		store.Set(k, v)
	}
	return store
}

func compareLines(t *testing.T, name, kind string, got, want [][3]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d %s, Node has %d\n got %v\nwant %v", name, len(got), kind, len(want), got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s: %s %d\n got %v\nwant %v", name, kind, i, got[i], want[i])
		}
	}
}

func TestPassMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	if len(g.Pass) == 0 {
		t.Fatal("no pass scenarios")
	}
	for _, sc := range g.Pass {
		db := &scriptDB{script: append([]scriptStep(nil), sc.Script...)}
		rec := &recorder{}
		j := &Job{DB: db, Config: storeOf(sc.Config), Log: rec}
		j.Pass(context.Background(), sc.Config["cleanup_cron_schedule"])

		if len(db.script) != sc.Unused {
			t.Errorf("%s: %d scripted statements unused, Node left %d", sc.Name, len(db.script), sc.Unused)
		}
		if len(db.queries) != len(sc.Queries) {
			t.Errorf("%s: %d statements, Node sent %d", sc.Name, len(db.queries), len(sc.Queries))
		} else {
			for i, q := range db.queries {
				want := sc.Queries[i]
				if fold(q.SQL) != fold(want.SQL) {
					t.Errorf("%s: statement %d\n got %s\nwant %s", sc.Name, i, fold(q.SQL), fold(want.SQL))
				}
				if fmt.Sprint(deref(q.Params)) != fmt.Sprint(deref(want.Params)) {
					t.Errorf("%s: statement %d params %v, Node %v", sc.Name, i, deref(q.Params), deref(want.Params))
				}
			}
		}
		compareLines(t, sc.Name, "log lines", rec.logs, sc.Logs)
		compareLines(t, sc.Name, "error_log calls", rec.errors, sc.Errors)
		// Node stored err.stack as the detail; Go stores its own text of
		// the error (never compared, as with every error_log row). It must
		// be there.
		for i, d := range rec.details {
			if d == "" {
				t.Errorf("%s: error_log call %d has no detail", sc.Name, i)
			}
		}
	}
}

func deref(params []*string) []string {
	out := make([]string, len(params))
	for i, p := range params {
		if p == nil {
			out[i] = "<null>"
		} else {
			out[i] = *p
		}
	}
	return out
}

// The statements match Node's; the transactions are the port's own. Each
// decay note is one transaction, and so is each retention stage.
func TestPassTransactions(t *testing.T) {
	g := loadGoldens(t)
	for _, sc := range g.Pass {
		if sc.Name != "full pass" {
			continue
		}
		db := &scriptDB{script: append([]scriptStep(nil), sc.Script...)}
		j := &Job{DB: db, Config: storeOf(sc.Config), Log: &recorder{}}
		j.Pass(context.Background(), "0 5 * * *")
		want := "SELECT BEGIN UPDATE INSERT DELETE COMMIT BEGIN UPDATE DELETE COMMIT DELETE " +
			"BEGIN UPDATE DELETE DELETE DELETE INSERT INSERT COMMIT BEGIN UPDATE DELETE COMMIT"
		if got := strings.Join(db.marks, " "); got != want {
			t.Errorf("marks\n got %s\nwant %s", got, want)
		}
		return
	}
	t.Fatal("no full pass scenario")
}

func TestStartMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Start {
		values := map[string]string{}
		if c.Schedule != nil {
			values["cleanup_cron_schedule"] = *c.Schedule
		}
		rec := &recorder{}
		j := &Job{DB: &scriptDB{}, Config: storeOf(values), Log: rec}
		task, err := j.Start()
		threw := ""
		if err != nil {
			threw = err.Error()
		}
		if task != nil {
			task.Drain(time.Second)
		}
		name := fmt.Sprintf("start %v", c.Schedule)
		if c.Schedule != nil {
			name = fmt.Sprintf("start %q", *c.Schedule)
		}
		if (task != nil) != c.Started || threw != c.Threw {
			t.Errorf("%s: started %v threw %q, Node %v %q", name, task != nil, threw, c.Started, c.Threw)
		}
		compareLines(t, name, "log lines", rec.logs, c.Logs)
	}
}
