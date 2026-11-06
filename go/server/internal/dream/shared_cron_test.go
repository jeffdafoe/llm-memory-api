package dream

// The tests of Node's dream-shared-cron.test.js, ported: the real Run and
// Pass over a database stand-in that answers by statement text, with the
// lock connection's events recorded. Three Node tests have no Go
// counterpart: "an error_log write failure does not change the run status"
// (LogError returns nothing; its insert runs in the background), and the
// two about a string or unparseable timestamp from the driver (pgx scans a
// timestamptz into a time.Time).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/cron"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/virtualagent"
)

const (
	pooledActorID    = 42
	pooledAgent      = "salem-vendor"
	dedicatedActorID = 73
	chunkFailure     = "simulated conversation-log query failure"
	unlockFailure    = "simulated pg_advisory_unlock failure"
	acquireFailure   = "simulated pg_try_advisory_lock failure"
	connectFailure   = "simulated pool.connect failure"
	runFailure       = "simulated agent-list query failure"
)

var testNow = time.Date(2026, 11, 6, 15, 30, 0, 0, time.UTC)

// midnightAlignedSince is UTC midnight daysBack days before testNow, so a
// window from it has exactly daysBack+1 chunks.
func midnightAlignedSince(daysBack int) time.Time {
	return time.Date(testNow.Year(), testNow.Month(), testNow.Day()-daysBack, 0, 0, 0, 0, time.UTC)
}

func expectedChunkCount(since time.Time) int {
	return int((testNow.Sub(since) + 24*time.Hour - 1) / (24 * time.Hour))
}

func sharedAgentRow() []any {
	return []any{pooledAgent, pooledActorID, "sim-shared", "conversation", nil, nil}
}

func dedicatedAgentRow(mode string, lastDreamAt any) []any {
	return []any{"prometheus", dedicatedActorID, mode, "conversation", lastDreamAt, nil}
}

func rosterRowOf(prefix, display string, lastDreamAt any) []any {
	return []any{prefix, display, lastDreamAt}
}

// routeDB is makeQueryStub: it answers by statement text and refuses any
// statement it does not know.
type routeDB struct {
	mu                 sync.Mutex
	agentRows          [][]any
	rosterRows         [][]any
	chunk              func(i int) ([][]any, error)
	notesChunk         func(i int) ([][]any, error)
	fallback           []any
	before, after      func(sql string)
	cursorUpdates      [][]any
	agentCursorUpdates [][]any
	notesChunkParams   [][]any
	chunkCount         int
	fallbackCount      int
}

func (d *routeDB) answer(sql string, args []any) ([][]any, error) {
	if d.before != nil {
		d.before(sql)
	}
	rows, err := d.route(sql, args)
	if d.after != nil {
		d.after(sql)
	}
	return rows, err
}

func (d *routeDB) route(sql string, args []any) ([][]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case strings.Contains(sql, "ac.expertise @> jsonb_build_array"):
		return [][]any{{900, args[0].(string) + "-va", 1, "openrouter", "test-model", "test-key"}}, nil
	case strings.Contains(sql, "FROM agent_configuration agc"):
		return d.agentRows, nil
	case strings.Contains(sql, "FROM sim_shared_actor"):
		return d.rosterRows, nil
	case strings.Contains(sql, "UPDATE sim_shared_actor SET last_dream_at"):
		d.cursorUpdates = append(d.cursorUpdates, args)
		return nil, nil
	case strings.Contains(sql, "UPDATE agent_configuration SET last_dream_at"):
		d.agentCursorUpdates = append(d.agentCursorUpdates, args)
		return nil, nil
	case strings.Contains(sql, "AS has_conversations") && d.fallback != nil:
		d.fallbackCount++
		return [][]any{d.fallback}, nil
	case strings.Contains(sql, "FROM documents") && strings.Contains(sql, "updated_at > $2") && d.notesChunk != nil:
		d.notesChunkParams = append(d.notesChunkParams, args)
		return d.notesChunk(len(d.notesChunkParams) - 1)
	case strings.Contains(sql, "FROM documents") && strings.Contains(sql, "created_at >"):
		// Counted before the answer, so a failing responder still advances.
		i := d.chunkCount
		d.chunkCount++
		return d.chunk(i)
	}
	return nil, errors.New("unexpected query in test: " + strings.TrimSpace(sql)[:min(120, len(strings.TrimSpace(sql)))])
}

func (d *routeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	_, err := d.answer(sql, args)
	return pgconn.NewCommandTag("UPDATE 1"), err
}

func (d *routeDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := d.answer(sql, args)
	if err != nil {
		return nil, err
	}
	return &typedRows{rows: rows, i: -1}, nil
}

func (d *routeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	rows, err := d.answer(sql, args)
	if err != nil {
		return errRow{err}
	}
	if len(rows) == 0 {
		return errRow{pgx.ErrNoRows}
	}
	return &typedRows{rows: rows[:1], i: 0}
}

func (d *routeDB) Begin(context.Context) (pgx.Tx, error) { return nil, errors.New("no transactions") }

type typedRows struct {
	pgx.Rows
	rows [][]any
	i    int
}

func (r *typedRows) Next() bool { r.i++; return r.i < len(r.rows) }
func (r *typedRows) Close()     {}
func (r *typedRows) Err() error { return nil }

func (r *typedRows) Scan(dest ...any) error {
	row := r.rows[r.i]
	if len(dest) != len(row) {
		return fmt.Errorf("scan %d values into %d destinations", len(row), len(dest))
	}
	for i, v := range row {
		switch d := dest[i].(type) {
		case *int:
			*d = v.(int)
		case **int:
			if v != nil {
				n := v.(int)
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
			*d = v.(time.Time)
		case **time.Time:
			if v != nil {
				ts := v.(time.Time)
				*d = &ts
			}
		default:
			return fmt.Errorf("unsupported destination %T", dest[i])
		}
	}
	return nil
}

// lockStub is makeLockStub: the connection holding the run lock, with its
// checkout, lock statements and release recorded in order.
type lockStub struct {
	mu           sync.Mutex
	events       []lockEvent
	granted      bool
	unlockFails  bool
	acquireFails bool
	unlockHeld   bool
	connectErr   bool
	lost         error
}

type lockEvent struct {
	kind      string
	key       any
	granted   bool
	destroyed bool
}

func newLockStub() *lockStub { return &lockStub{granted: true, unlockHeld: true} }

func (l *lockStub) record(e lockEvent) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
}

func (l *lockStub) kinds() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.events {
		out = append(out, e.kind)
	}
	return out
}

func (l *lockStub) find(kind string) *lockEvent {
	for i := range l.events {
		if l.events[i].kind == kind {
			return &l.events[i]
		}
	}
	return nil
}

// killConnection simulates the connection dropping mid-run.
func (l *lockStub) killConnection() {
	l.mu.Lock()
	l.lost = errors.New("simulated connection loss")
	l.mu.Unlock()
}

func (l *lockStub) Connect(context.Context) (Conn, error) {
	if l.connectErr {
		return nil, errors.New(connectFailure)
	}
	l.record(lockEvent{kind: "connect"})
	return l, nil
}

func (l *lockStub) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	if !strings.Contains(sql, "AS ok") {
		return errRow{errors.New("lock statement without AS ok: " + sql)}
	}
	switch {
	case strings.Contains(sql, "pg_try_advisory_lock"):
		if l.acquireFails {
			l.record(lockEvent{kind: "acquire-failed"})
			return errRow{errors.New(acquireFailure)}
		}
		l.record(lockEvent{kind: "acquire", key: args[0], granted: l.granted})
		return &typedRows{rows: [][]any{{l.granted}}, i: 0}
	case strings.Contains(sql, "pg_advisory_unlock"):
		l.record(lockEvent{kind: "unlock", key: args[0]})
		if l.unlockFails {
			return errRow{errors.New(unlockFailure)}
		}
		return &typedRows{rows: [][]any{{l.unlockHeld}}, i: 0}
	}
	return errRow{errors.New("unexpected lock statement")}
}

func (l *lockStub) Ping(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lost
}

func (l *lockStub) Release(destroy bool) { l.record(lockEvent{kind: "release", destroyed: destroy}) }

// eventLog records the dream log lines and the error_log calls.
type eventLog struct {
	mu     sync.Mutex
	events []struct {
		action  string
		details map[string]any
	}
	errors []logger.ErrorOpts
	errAct []string
}

func (l *eventLog) Log(_, action string, details logger.Fields) {
	b, _ := jsonutil.Marshal(details)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	l.mu.Lock()
	l.events = append(l.events, struct {
		action  string
		details map[string]any
	}{action, m})
	l.mu.Unlock()
}

func (l *eventLog) LogError(_, action string, opts logger.ErrorOpts) {
	l.mu.Lock()
	l.errors = append(l.errors, opts)
	l.errAct = append(l.errAct, action)
	l.mu.Unlock()
}

func (l *eventLog) Errorf(string, ...any) {}

func (l *eventLog) named(action string) []map[string]any {
	var out []map[string]any
	for _, e := range l.events {
		if e.action == action {
			out = append(out, e.details)
		}
	}
	return out
}

func (l *eventLog) errorNamed(action string) *logger.ErrorOpts {
	for i, a := range l.errAct {
		if a == action {
			return &l.errors[i]
		}
	}
	return nil
}

type noNotes struct{}

func (noNotes) Save(context.Context, notes.SaveInput) (notes.SavedNote, error) {
	return notes.SavedNote{}, errors.New("no note should be saved")
}
func (noNotes) Read(context.Context, string, string) (notes.Note, error) {
	return notes.Note{}, errors.New("not found")
}
func (noNotes) ListLive(context.Context, notes.ListInput) ([]notes.ListedNote, error) {
	return nil, nil
}

type noAgents struct{}

func (noAgents) Invoke(context.Context, string, virtualagent.InvokeOptions) (*virtualagent.InvokeResult, error) {
	return nil, errors.New("no model call should run")
}

type systemTrust struct{}

func (systemTrust) IsTrustedCreator(_ context.Context, id int) (bool, error) { return id == 1, nil }

type cronFixture struct {
	db   *routeDB
	lock *lockStub
	log  *eventLog
	job  *Job
}

func newCronFixture(db *routeDB) *cronFixture {
	f := &cronFixture{db: db, lock: newLockStub(), log: &eventLog{}}
	f.job = &Job{
		DB: db, Conns: f.lock, Log: f.log, Notes: noNotes{}, Agents: noAgents{}, Trust: systemTrust{},
		Config: storeOf(map[string]string{
			"dream_processing_enabled": "true",
			"dream_interagent_delay":   "1",
			"dream_interchunk_delay":   "1",
			"dream_cron_schedule":      "0 4 * * *",
		}),
		Now:   func() time.Time { return testNow },
		Sleep: func(context.Context, float64) error { return nil },
	}
	return f
}

func emptyChunks(int) ([][]any, error) { return nil, nil }

func failingChunks(int) ([][]any, error) { return nil, errors.New(chunkFailure) }

func value(f logger.Fields, key string) any {
	v, _ := get(f, key)
	return v
}

func sharedResult(t *testing.T, result logger.Fields) logger.Fields {
	t.Helper()
	for _, r := range value(result, "results").([]logger.Fields) {
		if value(r, "mode") == "sim-shared" {
			return r
		}
	}
	t.Fatal("no sim-shared result")
	return nil
}

func actorsOf(shared logger.Fields) []logger.Fields { return value(shared, "actors").([]logger.Fields) }

func chunksOf(r logger.Fields) []logger.Fields { return value(r, "chunks").([]logger.Fields) }

func run(t *testing.T, f *cronFixture) logger.Fields {
	t.Helper()
	result, err := f.job.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return result
}

func runFails(t *testing.T, f *cronFixture, want string) {
	t.Helper()
	_, err := f.job.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("run error %v, want %q", err, want)
	}
}

func TestSharedFailedChunkIsCountedAndStopsVillager(t *testing.T) {
	since := midnightAlignedSince(1)
	f := newCronFixture(&routeDB{
		agentRows:  [][]any{sharedAgentRow()},
		rosterRows: [][]any{rosterRowOf("constance-scott/", "Constance Scott", since)},
		chunk: func(i int) ([][]any, error) {
			if i == 0 {
				return nil, nil
			}
			return nil, errors.New(chunkFailure)
		},
	})
	result := run(t, f)
	if value(result, "failedSharedActorCount") != 1 {
		t.Errorf("failedSharedActorCount %v", value(result, "failedSharedActorCount"))
	}
	shared := sharedResult(t, result)
	if value(shared, "agent") != pooledAgent || value(shared, "actorCount") != 1 || value(shared, "failedActorCount") != 1 || value(shared, "error") != nil {
		t.Errorf("shared result %v", shared)
	}
	actor := actorsOf(shared)[0]
	if value(actor, "prefix") != "constance-scott/" || value(actor, "completedChunks") != 1 || value(actor, "plannedChunks") != expectedChunkCount(since) {
		t.Errorf("actor %v", actor)
	}
	chunks := chunksOf(actor)
	if len(chunks) != 2 || value(chunks[0], "skipped") != true || value(chunks[0], "reason") != "no logs" || value(chunks[1], "error") != chunkFailure {
		t.Errorf("chunks %v", chunks)
	}
	if len(f.db.cursorUpdates) != 1 || f.db.cursorUpdates[0][1] != pooledActorID || *(f.db.cursorUpdates[0][2].(*string)) != "constance-scott/" {
		t.Errorf("cursor updates %v", f.db.cursorUpdates)
	}
}

func TestOneFailingVillagerDoesNotFailTheRoster(t *testing.T) {
	since := midnightAlignedSince(1)
	f := newCronFixture(&routeDB{
		agentRows:  [][]any{sharedAgentRow()},
		rosterRows: [][]any{rosterRowOf("constance-scott/", "Constance Scott", since), rosterRowOf("josiah-thorne/", "Josiah Thorne", since)},
		chunk: func(i int) ([][]any, error) {
			if i == 0 {
				return nil, errors.New(chunkFailure)
			}
			return nil, nil
		},
	})
	result := run(t, f)
	shared := sharedResult(t, result)
	if value(shared, "actorCount") != 2 || value(shared, "failedActorCount") != 1 || value(result, "failedSharedActorCount") != 1 {
		t.Errorf("shared %v", shared)
	}
	constance, josiah := actorsOf(shared)[0], actorsOf(shared)[1]
	if value(constance, "completedChunks") != 0 || len(chunksOf(constance)) != 1 || value(chunksOf(constance)[0], "error") != chunkFailure {
		t.Errorf("constance %v", constance)
	}
	if value(josiah, "prefix") != "josiah-thorne/" || value(josiah, "completedChunks") != value(josiah, "plannedChunks") || value(josiah, "plannedChunks") != expectedChunkCount(since) {
		t.Errorf("josiah %v", josiah)
	}
	if len(f.db.cursorUpdates) != expectedChunkCount(since) {
		t.Errorf("%d cursor updates", len(f.db.cursorUpdates))
	}
	for _, u := range f.db.cursorUpdates {
		if *(u[2].(*string)) != "josiah-thorne/" {
			t.Errorf("cursor update for %v", *(u[2].(*string)))
		}
	}
}

func TestSchedulerReportsSharedFailure(t *testing.T) {
	f := newCronFixture(&routeDB{
		agentRows:  [][]any{sharedAgentRow()},
		rosterRows: [][]any{rosterRowOf("constance-scott/", "Constance Scott", midnightAlignedSince(1))},
		chunk:      failingChunks,
	})
	f.job.Pass(context.Background(), "0 4 * * *")
	complete := f.log.named("cron-complete")
	if len(complete) != 1 || complete[0]["status"] != "completed-with-errors" {
		t.Fatalf("cron-complete %v", complete)
	}
	if n := complete[0]["result"].(map[string]any)["failedSharedActorCount"]; n != 1.0 {
		t.Errorf("result failedSharedActorCount %v", n)
	}
	record := f.log.errorNamed("cron-shared-actor-failures")
	if record == nil || !strings.HasPrefix(record.Message, "1 shared-VA actor(s) failed") {
		t.Errorf("cron-shared-actor-failures %+v", record)
	}
	// The chunk that caused it is recorded on its own.
	if chunk := f.log.errorNamed("chunk-error"); chunk == nil || chunk.Message != chunkFailure {
		t.Errorf("chunk-error %+v", chunk)
	}
}

func TestCleanSharedRunReportsOK(t *testing.T) {
	f := newCronFixture(&routeDB{
		agentRows:  [][]any{sharedAgentRow()},
		rosterRows: [][]any{rosterRowOf("constance-scott/", "Constance Scott", midnightAlignedSince(1))},
		chunk:      emptyChunks,
	})
	f.job.Pass(context.Background(), "0 4 * * *")
	complete := f.log.named("cron-complete")
	if len(complete) != 1 || complete[0]["status"] != "ok" || complete[0]["result"].(map[string]any)["failedSharedActorCount"] != 0.0 {
		t.Errorf("cron-complete %v", complete)
	}
	if len(f.log.errors) != 0 {
		t.Errorf("error_log calls %v", f.log.errAct)
	}
}

func TestEmptyRosterIsCleanRun(t *testing.T) {
	f := newCronFixture(&routeDB{
		agentRows: [][]any{sharedAgentRow()},
		chunk: func(int) ([][]any, error) {
			return nil, errors.New("no chunk query should run for an empty roster")
		},
	})
	result := run(t, f)
	shared := sharedResult(t, result)
	if value(result, "failedSharedActorCount") != 0 || value(shared, "actorCount") != 0 || value(shared, "failedActorCount") != nil || f.db.chunkCount != 0 {
		t.Errorf("result %v", result)
	}
}

func TestUnusablePrefixFailsOnlyThatVillager(t *testing.T) {
	f := newCronFixture(&routeDB{
		agentRows: [][]any{sharedAgentRow()},
		rosterRows: [][]any{
			rosterRowOf("constance-scott", "Constance Scott", midnightAlignedSince(1)),
			rosterRowOf("josiah-thorne/", "Josiah Thorne", midnightAlignedSince(1)),
		},
		chunk: emptyChunks,
	})
	result := run(t, f)
	shared := sharedResult(t, result)
	actors := actorsOf(shared)
	if value(shared, "actorCount") != 2 || value(shared, "failedActorCount") != 1 || value(result, "failedSharedActorCount") != 1 ||
		value(actors[0], "error") != "invalid slug prefix" || value(actors[1], "prefix") != "josiah-thorne/" || value(actors[1], "completedChunks").(int) == 0 {
		t.Errorf("shared %v", shared)
	}
}

func TestSecondRunFindsLockHeld(t *testing.T) {
	f := newCronFixture(&routeDB{before: func(sql string) { panic("no query should run while the lock is held: " + sql) }})
	f.lock.granted = false
	result := run(t, f)
	if jsonutilString(result) != `{"skipped":true,"reason":"already running"}` {
		t.Errorf("result %s", jsonutilString(result))
	}
	if got := strings.Join(f.lock.kinds(), ","); got != "connect,acquire,release" {
		t.Errorf("lock events %s", got)
	}
	if f.lock.events[1].granted || f.lock.events[2].destroyed {
		t.Errorf("lock events %+v", f.lock.events)
	}
	skip := f.log.named("skip")
	if len(skip) != 1 || skip[0]["reason"] != "another dream run holds the lock" || len(f.log.named("error")) != 0 {
		t.Errorf("logs %v", f.log.events)
	}
}

func jsonutilString(f logger.Fields) string {
	b, _ := jsonutil.Marshal(f)
	return string(b)
}

func TestLockBracketsTheRun(t *testing.T) {
	db := &routeDB{
		agentRows:  [][]any{sharedAgentRow()},
		rosterRows: [][]any{rosterRowOf("constance-scott/", "Constance Scott", midnightAlignedSince(1))},
		chunk:      emptyChunks,
	}
	f := newCronFixture(db)
	db.before = func(string) { f.lock.record(lockEvent{kind: "query"}) }
	result := run(t, f)
	if value(result, "failedSharedActorCount") != 0 || len(db.cursorUpdates) == 0 {
		t.Fatalf("result %v", result)
	}
	kinds := f.lock.kinds()
	if strings.Join(kinds[:2], ",") != "connect,acquire" || strings.Join(kinds[len(kinds)-2:], ",") != "unlock,release" {
		t.Errorf("lock events %v", kinds)
	}
	first, last := -1, -1
	for i, k := range kinds {
		if k == "query" {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 2 || last > len(kinds)-3 {
		t.Errorf("a query ran outside the lock: %v", kinds)
	}
	if f.lock.find("acquire").key != f.lock.find("unlock").key || f.lock.find("release").destroyed {
		t.Errorf("lock events %+v", f.lock.events)
	}
}

func TestThrowingRunStillReleases(t *testing.T) {
	db := &routeDB{chunk: emptyChunks}
	f := newCronFixture(db)
	// The agent list fails.
	f.job.DB = failingAgentsDB{db}
	runFails(t, f, runFailure)
	if got := strings.Join(f.lock.kinds(), ","); got != "connect,acquire,unlock,release" {
		t.Errorf("lock events %s", got)
	}
}

type failingAgentsDB struct{ *routeDB }

func (d failingAgentsDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM agent_configuration agc") {
		return nil, errors.New(runFailure)
	}
	return d.routeDB.Query(ctx, sql, args...)
}

func TestFailedUnlockDestroysTheConnection(t *testing.T) {
	f := newCronFixture(&routeDB{agentRows: [][]any{sharedAgentRow()}, chunk: emptyChunks})
	f.lock.unlockFails = true
	result := run(t, f)
	if value(result, "failedSharedActorCount") != 0 || !f.lock.find("release").destroyed {
		t.Errorf("result %v, lock %+v", result, f.lock.events)
	}
	failure := f.log.named("lock-release-error")
	if len(failure) != 1 || failure[0]["error"] != unlockFailure {
		t.Errorf("lock-release-error %v", failure)
	}
}

func TestLockLostAbortsBeforeTheNextCursorWrite(t *testing.T) {
	since := midnightAlignedSince(3)
	db := &routeDB{
		agentRows:  [][]any{sharedAgentRow()},
		rosterRows: [][]any{rosterRowOf("constance-scott/", "Constance Scott", since)},
		chunk:      emptyChunks,
	}
	f := newCronFixture(db)
	db.after = func(sql string) {
		if strings.Contains(sql, "UPDATE sim_shared_actor SET last_dream_at") && len(db.cursorUpdates) == 1 {
			f.lock.killConnection()
		}
	}
	if expectedChunkCount(since) <= 1 {
		t.Fatal("need a multi-chunk window")
	}
	runFails(t, f, "dream run lock lost")
	if len(db.cursorUpdates) != 1 {
		t.Errorf("%d cursor updates", len(db.cursorUpdates))
	}
	if len(f.log.named("lock-connection-error")) == 0 {
		t.Error("the connection loss was not logged")
	}
	if got := strings.Join(f.lock.kinds(), ","); got != "connect,acquire,unlock,release" {
		t.Errorf("lock events %s", got)
	}
}

func TestLockLostDuringChunkKeepsTheCursor(t *testing.T) {
	db := &routeDB{
		agentRows:  [][]any{sharedAgentRow()},
		rosterRows: [][]any{rosterRowOf("constance-scott/", "Constance Scott", midnightAlignedSince(1))},
		chunk:      emptyChunks,
	}
	f := newCronFixture(db)
	db.before = func(sql string) {
		if strings.Contains(sql, "FROM documents") && strings.Contains(sql, "created_at >") {
			f.lock.killConnection()
		}
	}
	runFails(t, f, "dream run lock lost")
	if len(db.cursorUpdates) != 0 {
		t.Errorf("%d cursor updates after the lock was lost", len(db.cursorUpdates))
	}
}

func TestCleanRunUnlocksAsHeld(t *testing.T) {
	f := newCronFixture(&routeDB{
		agentRows:  [][]any{sharedAgentRow()},
		rosterRows: [][]any{rosterRowOf("constance-scott/", "Constance Scott", midnightAlignedSince(1))},
		chunk:      emptyChunks,
	})
	result := run(t, f)
	if value(result, "failedSharedActorCount") != 0 || len(f.log.named("lock-release-not-held")) != 0 || len(f.log.named("lock-release-error")) != 0 {
		t.Errorf("logs %v", f.log.events)
	}
}

func TestLossAfterTheWorkFailsTheRun(t *testing.T) {
	db := &routeDB{agentRows: [][]any{sharedAgentRow()}, chunk: emptyChunks}
	f := newCronFixture(db)
	db.after = func(sql string) {
		if strings.Contains(sql, "FROM sim_shared_actor") {
			f.lock.killConnection()
		}
	}
	runFails(t, f, "dream run lock lost")
	if got := strings.Join(f.lock.kinds(), ","); got != "connect,acquire,unlock,release" {
		t.Errorf("lock events %s", got)
	}
}

func TestUnlockNotHeldIsLogged(t *testing.T) {
	f := newCronFixture(&routeDB{agentRows: [][]any{sharedAgentRow()}, chunk: emptyChunks})
	f.lock.unlockHeld = false
	run(t, f)
	if len(f.log.named("lock-release-not-held")) != 1 {
		t.Error("a false pg_advisory_unlock result was ignored")
	}
}

func TestNoConnectionFailsWithoutQueries(t *testing.T) {
	f := newCronFixture(&routeDB{before: func(sql string) { panic("no query should run: " + sql) }})
	f.lock.connectErr = true
	runFails(t, f, connectFailure)
}

func TestFailedAcquisitionDestroysTheConnection(t *testing.T) {
	f := newCronFixture(&routeDB{before: func(sql string) { panic("no query should run: " + sql) }})
	f.lock.acquireFails = true
	runFails(t, f, acquireFailure)
	if got := strings.Join(f.lock.kinds(), ","); got != "connect,acquire-failed,release" || !f.lock.events[2].destroyed {
		t.Errorf("lock events %+v", f.lock.events)
	}
}

func TestDisabledRunTakesNoConnection(t *testing.T) {
	f := newCronFixture(&routeDB{before: func(sql string) { panic("no query should run: " + sql) }})
	f.job.Config = storeOf(map[string]string{"dream_processing_enabled": "false"})
	result := run(t, f)
	if jsonutilString(result) != `{"skipped":true,"reason":"disabled"}` || len(f.lock.events) != 0 {
		t.Errorf("result %s, lock %v", jsonutilString(result), f.lock.events)
	}
}

func TestSharedAgentOnNotesSourceIsRejected(t *testing.T) {
	row := sharedAgentRow()
	row[3] = "notes"
	f := newCronFixture(&routeDB{
		agentRows:  [][]any{row},
		rosterRows: [][]any{rosterRowOf("constance-scott/", "Constance Scott", midnightAlignedSince(1))},
		chunk: func(int) ([][]any, error) {
			return nil, errors.New("no chunk query should run for a rejected agent")
		},
	})
	result := run(t, f)
	shared := sharedResult(t, result)
	if value(result, "failedSharedActorCount") != 1 || value(shared, "failedActorCount") != 1 ||
		!strings.Contains(value(shared, "error").(string), "dream_source must be conversation") || f.db.chunkCount != 0 {
		t.Errorf("result %v", result)
	}
}

func TestNotesFallbackBackfillsFromTheEarliestNote(t *testing.T) {
	earliest := midnightAlignedSince(3).Add(5 * time.Hour)
	db := &routeDB{
		agentRows: [][]any{dedicatedAgentRow("technical", midnightAlignedSince(1))},
		chunk: func(int) ([][]any, error) {
			return nil, errors.New("the conversation-log query must not run in fallback")
		},
		fallback:   []any{false, false, earliest},
		notesChunk: emptyChunks,
	}
	f := newCronFixture(db)
	run(t, f)
	if got := f.log.named("source-fallback"); len(got) != 1 || got[0]["agent"] != "prometheus" || got[0]["from"] != "conversation" || got[0]["to"] != "notes" {
		t.Errorf("source-fallback %v", got)
	}
	if from := db.notesChunkParams[0][1].(time.Time); !from.Equal(earliest.Add(-time.Millisecond)) || db.notesChunkParams[0][0] != "prometheus" {
		t.Errorf("first window %v", db.notesChunkParams[0])
	}
	for _, p := range db.notesChunkParams {
		if p[3] != "note" {
			t.Errorf("kind filter %v", p[3])
		}
	}
	noLogs := f.log.named("chunk-no-logs")
	if len(noLogs) < 3 || db.chunkCount != 0 || len(db.agentCursorUpdates) != len(noLogs) {
		t.Errorf("%d no-logs, %d chunk queries, %d cursor updates", len(noLogs), db.chunkCount, len(db.agentCursorUpdates))
	}
	for _, e := range noLogs {
		if e["source"] != "notes" {
			t.Errorf("chunk-no-logs %v", e)
		}
	}
}

func TestFallbackWithADreamKeepsItsCursor(t *testing.T) {
	cursor := midnightAlignedSince(1)
	db := &routeDB{
		agentRows: [][]any{dedicatedAgentRow("companion", cursor)},
		chunk: func(int) ([][]any, error) {
			return nil, errors.New("the conversation-log query must not run in fallback")
		},
		fallback:   []any{false, true, midnightAlignedSince(20)},
		notesChunk: emptyChunks,
	}
	f := newCronFixture(db)
	run(t, f)
	if len(f.log.named("source-fallback")) != 1 || !db.notesChunkParams[0][1].(time.Time).Equal(cursor) {
		t.Errorf("window %v", db.notesChunkParams[0])
	}
}

func TestAgentWithLogsIsNotSwitched(t *testing.T) {
	db := &routeDB{
		agentRows: [][]any{dedicatedAgentRow("technical", midnightAlignedSince(1))},
		chunk:     emptyChunks,
		fallback:  []any{true, true, midnightAlignedSince(5)},
		notesChunk: func(int) ([][]any, error) {
			return nil, errors.New("the notes query must not run for an agent with logs")
		},
	}
	f := newCronFixture(db)
	run(t, f)
	if len(f.log.named("source-fallback")) != 0 || db.chunkCount < 1 {
		t.Errorf("fallback %v, %d chunk queries", f.log.named("source-fallback"), db.chunkCount)
	}
	for _, e := range f.log.named("chunk-no-logs") {
		if e["source"] != "conversation" {
			t.Errorf("chunk-no-logs %v", e)
		}
	}
}

func TestSignupTemplateOnlyIsNotSwitched(t *testing.T) {
	db := &routeDB{
		agentRows: [][]any{dedicatedAgentRow("technical", midnightAlignedSince(1))},
		chunk:     emptyChunks,
		fallback:  []any{false, false, nil},
		notesChunk: func(int) ([][]any, error) {
			return nil, errors.New("the notes query must not run for an account with no notes of its own")
		},
	}
	f := newCronFixture(db)
	run(t, f)
	if len(f.log.named("source-fallback")) != 0 || db.chunkCount < 1 {
		t.Errorf("fallback %v", f.log.named("source-fallback"))
	}
}

func TestExplicitNotesAgentIsNotProbed(t *testing.T) {
	row := dedicatedAgentRow("companion", midnightAlignedSince(1))
	row[3] = "notes"
	db := &routeDB{
		agentRows: [][]any{row},
		chunk: func(int) ([][]any, error) {
			return nil, errors.New("the conversation-log query must not run for a notes agent")
		},
		fallback:   []any{false, false, midnightAlignedSince(5)},
		notesChunk: emptyChunks,
	}
	f := newCronFixture(db)
	run(t, f)
	if db.fallbackCount != 0 || len(f.log.named("source-fallback")) != 0 || len(db.notesChunkParams) < 1 {
		t.Errorf("probed %d, chunks %d", db.fallbackCount, len(db.notesChunkParams))
	}
	for _, p := range db.notesChunkParams {
		if p[3] != nil {
			t.Errorf("kind filter %v", p[3])
		}
	}
}

func TestSimAgentIsNotProbed(t *testing.T) {
	row := dedicatedAgentRow("sim", midnightAlignedSince(1))
	row[0] = "zbbs-john-ellis"
	db := &routeDB{
		agentRows: [][]any{row},
		chunk:     emptyChunks,
		fallback:  []any{false, false, midnightAlignedSince(5)},
		notesChunk: func(int) ([][]any, error) {
			return nil, errors.New("the notes query must not run for a sim agent")
		},
	}
	f := newCronFixture(db)
	run(t, f)
	if db.fallbackCount != 0 || len(f.log.named("source-fallback")) != 0 || db.chunkCount < 1 {
		t.Errorf("probed %d", db.fallbackCount)
	}
}

func TestSharedAgentIsNotProbed(t *testing.T) {
	db := &routeDB{
		agentRows:  [][]any{sharedAgentRow()},
		rosterRows: [][]any{rosterRowOf("constance-scott/", "Constance Scott", midnightAlignedSince(1))},
		chunk:      emptyChunks,
		fallback:   []any{false, false, midnightAlignedSince(5)},
		notesChunk: func(int) ([][]any, error) {
			return nil, errors.New("the notes query must not run for a sim-shared agent")
		},
	}
	f := newCronFixture(db)
	run(t, f)
	if db.fallbackCount != 0 || len(f.log.named("source-fallback")) != 0 {
		t.Errorf("probed %d", db.fallbackCount)
	}
}

// A run that shutdown cancels stops at the next boundary and still
// releases the lock (Known differences: Node had no graceful shutdown).
func TestCancelledRunStopsAndReleases(t *testing.T) {
	db := &routeDB{
		agentRows:  [][]any{sharedAgentRow()},
		rosterRows: [][]any{rosterRowOf("a/", "A", midnightAlignedSince(3)), rosterRowOf("b/", "B", midnightAlignedSince(3))},
		chunk:      emptyChunks,
	}
	f := newCronFixture(db)
	ctx, cancel := context.WithCancel(context.Background())
	db.after = func(sql string) {
		if strings.Contains(sql, "UPDATE sim_shared_actor") {
			cancel()
		}
	}
	_, err := f.job.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if len(db.cursorUpdates) != 1 {
		t.Errorf("%d cursor updates after the cancel", len(db.cursorUpdates))
	}
	if got := strings.Join(f.lock.kinds(), ","); got != "connect,acquire,unlock,release" {
		t.Errorf("lock events %s", got)
	}
}

// Review round 1: a cancel after the last cursor write, with no pause
// after the agent, fails the run instead of reporting it clean.
func TestCancelAfterTheLastCursorFailsTheRun(t *testing.T) {
	db := &routeDB{agentRows: [][]any{dedicatedAgentRow("technical", midnightAlignedSince(0))}, chunk: emptyChunks, fallback: []any{true, false, nil}}
	f := newCronFixture(db)
	f.job.Config.(interface{ Set(string, string) }).Set("dream_interagent_delay", "0")
	ctx, cancel := context.WithCancel(context.Background())
	db.after = func(sql string) {
		if strings.Contains(sql, "UPDATE agent_configuration SET last_dream_at") {
			cancel()
		}
	}
	_, err := f.job.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
	if len(db.agentCursorUpdates) != 1 {
		t.Errorf("%d cursor updates", len(db.agentCursorUpdates))
	}
	if got := strings.Join(f.lock.kinds(), ","); got != "connect,acquire,unlock,release" {
		t.Errorf("lock events %s", got)
	}
}

// Review round 1: the signal budget is one per line, shared by the
// patterns, and running out means no signal, even when a later pattern
// would have matched.
func TestSignalBudgetIsPerLine(t *testing.T) {
	line := strings.Repeat("x", 5000) + " miss"
	if !hasSignal(line) {
		t.Fatal("the line has a signal under the real budget")
	}
	saved := signalBudget
	defer func() { signalBudget = saved }()
	signalBudget = 20000
	if hasSignal(line) {
		t.Error("an exhausted budget still found the last pattern's signal")
	}
	if hasSignal(strings.Repeat("y", 50000)) {
		t.Error("an exhausted budget read as a signal")
	}
	if !hasSignal("remember") {
		t.Error("a short line ran out of budget")
	}
}

// Review round 1: starting again with a valid schedule replaces the
// running one; an empty or refused schedule leaves it running.
func TestStartReplacesTheRunningSchedule(t *testing.T) {
	var stopped []*cron.Task
	saved := stopTask
	defer func() { stopTask = saved }()
	stopTask = func(task *cron.Task) { stopped = append(stopped, task); task.Stop() }

	store := storeOf(map[string]string{"dream_cron_schedule": "0 4 * * *"})
	j := &Job{Config: store, Log: &eventLog{}}
	first, err := j.Start()
	if err != nil || first == nil {
		t.Fatalf("first start: %v %v", first, err)
	}
	defer first.Stop()
	for _, schedule := range []string{"", "not a cron"} {
		store.Set("dream_cron_schedule", schedule)
		if task, err := j.Start(); task != nil || err != nil {
			t.Fatalf("start %q: %v %v", schedule, task, err)
		}
	}
	if len(stopped) != 0 {
		t.Fatalf("an empty or refused schedule stopped the running one")
	}
	store.Set("dream_cron_schedule", "0 5 * * *")
	second, err := j.Start()
	if err != nil || second == nil {
		t.Fatalf("second start: %v %v", second, err)
	}
	defer second.Stop()
	if len(stopped) != 1 || stopped[0] != first {
		t.Errorf("stopped %v, want the first task", stopped)
	}
}
