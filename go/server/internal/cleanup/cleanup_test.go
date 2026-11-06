package cleanup

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

var halfLifeKeys = []string{
	"search_decay_halflife_task", "search_decay_halflife_learning", "search_decay_halflife_note",
	"search_decay_halflife_conversation", "search_decay_halflife_dream",
	"search_decay_halflife_episodic", "search_decay_halflife_reflective",
}

// silenced is "nothing decays" plus the keys a case sets, as the Node tests
// start each case.
func silenced(values map[string]string) *Job {
	all := map[string]string{}
	for _, k := range halfLifeKeys {
		all[k] = "0"
	}
	for k, v := range values {
		all[k] = v
	}
	return &Job{Config: storeOf(all), Log: &recorder{}}
}

func conditionsOf(t *testing.T, j *Job) ([]string, []any) {
	t.Helper()
	conditions, params, err := j.BuildDecayConditions(0.05)
	if err != nil {
		t.Fatal(err)
	}
	return conditions, params
}

func mentions(conditions []string, needle string) bool {
	for _, c := range conditions {
		if strings.Contains(c, needle) {
			return true
		}
	}
	return false
}

func paramsEqual(got []any, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i, g := range got {
		if g != jsutil.Number(want[i]) {
			return false
		}
	}
	return true
}

// The cases below are node/api/src/services/cleanup.test.js.

func TestZeroHalfLifeGivesNoCondition(t *testing.T) {
	conditions, _ := conditionsOf(t, silenced(nil))
	if len(conditions) != 0 {
		t.Fatalf("conditions %v", conditions)
	}
}

func TestExplicitZeroDisablesCognitiveDefaults(t *testing.T) {
	conditions, params := conditionsOf(t, silenced(map[string]string{
		"search_decay_halflife_episodic": "0", "search_decay_halflife_reflective": "0",
	}))
	if len(conditions) != 0 || !paramsEqual(params, "0.05") {
		t.Fatalf("conditions %v params %v", conditions, params)
	}
}

func TestSilencingOneCognitiveTypeKeepsTheOther(t *testing.T) {
	conditions, params := conditionsOf(t, silenced(map[string]string{
		"search_decay_halflife_episodic": "0", "search_decay_halflife_reflective": "180",
	}))
	if len(conditions) != 1 || !mentions(conditions, "= 'reflective'") || mentions(conditions, "= 'episodic'") || !paramsEqual(params, "0.05", "180") {
		t.Fatalf("conditions %v params %v", conditions, params)
	}
}

func TestNonZeroHalfLifeIsBound(t *testing.T) {
	conditions, params := conditionsOf(t, silenced(map[string]string{"search_decay_halflife_note": "45"}))
	if len(conditions) != 1 || !mentions(conditions, "d.kind = 'note'") || !paramsEqual(params, "0.05", "45") {
		t.Fatalf("conditions %v params %v", conditions, params)
	}
}

func TestBlankOrGarbageHalfLifeFallsBack(t *testing.T) {
	// Node's case also left episodic undefined; a Go store has no undefined
	// value, and a missing key is an error (TestMissingHalfLifeKey).
	conditions, params := conditionsOf(t, silenced(map[string]string{
		"search_decay_halflife_note": "", "search_decay_halflife_dream": "soon", "search_decay_halflife_episodic": "",
	}))
	if len(conditions) != 1 || !mentions(conditions, "= 'episodic'") || !paramsEqual(params, "0.05", "90") {
		t.Fatalf("conditions %v params %v", conditions, params)
	}
}

func TestBlankCognitiveHalfLifeFallsBackToDefault(t *testing.T) {
	conditions, params := conditionsOf(t, silenced(map[string]string{
		"search_decay_halflife_episodic": "", "search_decay_halflife_reflective": "   ",
	}))
	if len(conditions) != 2 || !paramsEqual(params, "0.05", "90", "180") {
		t.Fatalf("conditions %v params %v", conditions, params)
	}
}

func TestNegativeHalfLifeDisablesTheRule(t *testing.T) {
	conditions, params := conditionsOf(t, silenced(map[string]string{
		"search_decay_halflife_note": "-45", "search_decay_halflife_reflective": "-1",
	}))
	if len(conditions) != 0 || !paramsEqual(params, "0.05") {
		t.Fatalf("conditions %v params %v", conditions, params)
	}
}

func TestPlaceholdersStayAligned(t *testing.T) {
	conditions, params := conditionsOf(t, silenced(map[string]string{
		"search_decay_halflife_note": "45", "search_decay_halflife_dream": "30", "search_decay_halflife_episodic": "90",
	}))
	if !paramsEqual(params, "0.05", "45", "30", "90") {
		t.Fatalf("params %v", params)
	}
	has := func(kind, placeholder string) bool {
		for _, c := range conditions {
			if strings.Contains(c, kind) && strings.Contains(c, placeholder) {
				return true
			}
		}
		return false
	}
	if !has("d.kind = 'note'", "$2") || !has("d.kind = 'dream'", "$3") || !has("= 'episodic'", "$4") {
		t.Fatalf("conditions %v", conditions)
	}
}

func TestMissingHalfLifeKey(t *testing.T) {
	j := &Job{Config: storeOf(map[string]string{"search_decay_halflife_task": "1"}), Log: &recorder{}}
	_, _, err := j.BuildDecayConditions(0.05)
	if err == nil || err.Error() != `Config key "search_decay_halflife_learning" not found. Was config.init() called?` {
		t.Fatalf("err %v", err)
	}
}

func TestRetentionWindows(t *testing.T) {
	if w := ConversationRetentionWindows("30"); w == nil || *w != (Windows{30, 31, 31}) {
		t.Fatalf("30: %+v", w)
	}
	if w := ConversationRetentionWindows("0"); w == nil || *w != (Windows{0, 1, 1}) {
		t.Fatalf("0: %+v", w)
	}
	for _, raw := range []string{"", "   ", "abc", "30.5", "-5", "Infinity"} {
		if w := ConversationRetentionWindows(raw); w != nil {
			t.Errorf("%q: %+v, want off", raw, w)
		}
	}
}

// The transaction paths, which the Node goldens cannot show.

const stage1Match = `SET deleted_at = NOW\(\)\s+WHERE kind = 'conversation'`

func retentionJob(mock pgxmock.PgxPoolIface, rec *recorder) *Job {
	return &Job{DB: mock, Config: storeOf(map[string]string{"conversation_retention_days": "30"}), Log: rec}
}

func TestRetentionChunkFailureRollsBackTheStage(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.MatchExpectationsInOrder(true)
	mock.ExpectBegin()
	mock.ExpectQuery(stage1Match).WithArgs(jsutil.Number("31")).
		WillReturnRows(pgxmock.NewRows([]string{"namespace", "slug", "content_length"}).
			AddRow("agent-a", "conversations/s1", int64Ptr(10)).
			AddRow("agent-a", "conversations/s2", int64Ptr(20)))
	mock.ExpectExec(regexp.QuoteMeta(chunksSQL)).WithArgs("agent-a", "conversations/s1").WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(regexp.QuoteMeta(chunksSQL)).WithArgs("agent-a", "conversations/s2").
		WillReturnError(&pgconn.PgError{Message: "could not serialize access"})
	mock.ExpectRollback()

	rec := &recorder{}
	_, err := retentionJob(mock, rec).RetireConversations(context.Background())
	if err == nil || message(err) != "could not serialize access" {
		t.Fatalf("err %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	want := [3]string{"cleanup", "conversation-chunk-error", `{"namespace":"agent-a","slug":"conversations/s2","error":"could not serialize access"}`}
	if len(rec.logs) != 1 || rec.logs[0] != want {
		t.Fatalf("logs %v", rec.logs)
	}
}

func TestRetentionUsageFailureRollsBackTheStage(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.MatchExpectationsInOrder(true)
	mock.ExpectBegin()
	mock.ExpectQuery(stage1Match).WithArgs(jsutil.Number("31")).
		WillReturnRows(pgxmock.NewRows([]string{"namespace", "slug", "content_length"}).AddRow("agent-a", "conversations/s1", (*int64)(nil)))
	mock.ExpectExec(regexp.QuoteMeta(chunksSQL)).WithArgs("agent-a", "conversations/s1").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec("INSERT INTO namespace_usage").WithArgs("agent-a", int64(-1), int64(0)).WillReturnError(errors.New("connection reset"))
	mock.ExpectRollback()

	_, err := retentionJob(mock, &recorder{}).RetireConversations(context.Background())
	if err == nil || err.Error() != "connection reset" {
		t.Fatalf("err %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDecayNoteFailureRollsBackThatNoteOnly(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.MatchExpectationsInOrder(true)
	j := &Job{DB: mock, Config: storeOf(map[string]string{}), Log: &recorder{}}

	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE documents SET deleted_at = NOW").WithArgs(int64(7)).
		WillReturnRows(pgxmock.NewRows([]string{"content_length"}).AddRow(int64Ptr(40)))
	mock.ExpectExec("INSERT INTO namespace_usage").WithArgs("ns", -1, int64(-40)).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(regexp.QuoteMeta(chunksSQL)).WithArgs("ns", "notes/a").WillReturnError(errors.New("disk full"))
	mock.ExpectRollback()
	if err := j.retireNote(context.Background(), decayRow{id: 7, namespace: "ns", slug: "notes/a"}); err == nil {
		t.Fatal("no error")
	}

	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE documents SET deleted_at = NOW").WithArgs(int64(8)).
		WillReturnRows(pgxmock.NewRows([]string{"content_length"}))
	mock.ExpectExec(regexp.QuoteMeta(chunksSQL)).WithArgs("ns", "notes/b").WillReturnResult(pgxmock.NewResult("DELETE", 3))
	mock.ExpectCommit()
	if err := j.retireNote(context.Background(), decayRow{id: 8, namespace: "ns", slug: "notes/b"}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func int64Ptr(n int64) *int64 { return &n }

func TestCancelledPassStopsAtOnce(t *testing.T) {
	g := loadGoldens(t)
	var full *scriptStep
	for _, sc := range g.Pass {
		if sc.Name == "full pass" {
			full = &sc.Script[0]
			break
		}
	}
	if full == nil {
		t.Fatal("no full pass scenario")
	}
	db := &scriptDB{script: []scriptStep{*full}}
	rec := &recorder{}
	j := &Job{DB: db, Config: storeOf(map[string]string{
		"cleanup_enabled": "true", "cleanup_decay_threshold": "0.05",
		"search_decay_halflife_task": "0", "search_decay_halflife_learning": "0", "search_decay_halflife_note": "180",
		"search_decay_halflife_conversation": "0", "search_decay_halflife_dream": "0",
		"search_decay_halflife_episodic": "0", "search_decay_halflife_reflective": "0",
		"va_call_log_retention_days": "5", "conversation_retention_days": "30",
	}), Log: rec}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	j.Pass(ctx, "0 5 * * *")
	if len(db.queries) != 1 {
		t.Fatalf("%d statements after the cancel, want only the candidate query", len(db.queries))
	}
	last := rec.logs[len(rec.logs)-1]
	if last != [3]string{"cleanup", "cron-decay-error", `{"error":"context canceled"}`} || len(rec.errors) != 1 {
		t.Fatalf("logs %v errors %v", rec.logs, rec.errors)
	}
}
