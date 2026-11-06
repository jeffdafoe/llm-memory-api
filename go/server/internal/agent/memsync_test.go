package agent

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// The Node tests in routes/agent.test.js, ported.

var pruneNow = float64(time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC).UnixMilli())

func TestResolvePruneCutoff(t *testing.T) {
	cases := []struct {
		name  string
		in    any
		want  float64
		valid bool
	}{
		{"places the cutoff on the server clock", json.Number("60000"), pruneNow - 60000, true},
		{"accepts zero", json.Number("0"), pruneNow, true},
		{"accepts the ceiling", json.Number("3600000"), pruneNow - 3600000, true},
		{"accepts a fraction", json.Number("0.5"), pruneNow - 0.5, true},
		{"undefined", jsutil.Undefined, 0, false},
		{"null", nil, 0, false},
		{"a numeric string", "60000", 0, false},
		{"NaN", math.NaN(), 0, false},
		{"Infinity", json.Number("1e400"), 0, false},
		{"negative", json.Number("-1"), 0, false},
		{"past the ceiling", json.Number("3600001"), 0, false},
		{"true", true, 0, false},
	}
	for _, c := range cases {
		got, ok := resolvePruneCutoff(c.in, pruneNow)
		if ok != c.valid || (ok && got != c.want) {
			t.Errorf("%s: %v %v", c.name, got, ok)
		}
	}
}

func TestRemoteOnlyAction(t *testing.T) {
	cases := []struct {
		name    string
		prune   bool
		updated *string
		want    string
	}{
		{"outside prune mode a remote-only note pulls", false, strPtr("2026-07-30T09:00:00.000Z"), "pull"},
		{"a note older than the scan prunes", true, strPtr("2026-07-30T09:00:00.000Z"), "prune"},
		{"a note updated exactly at the cutoff prunes", true, strPtr("2026-07-30T12:00:00.000Z"), "prune"},
		{"a note updated after the scan pulls", true, strPtr("2026-07-30T12:00:01.000Z"), "pull"},
		{"a missing time keeps the note", true, nil, "pull"},
		{"an unparseable time keeps the note", true, strPtr("garbage"), "pull"},
	}
	for _, c := range cases {
		if got := remoteOnlyAction(c.prune, c.updated, pruneNow, time.UTC); got != c.want {
			t.Errorf("%s: %s", c.name, got)
		}
	}
}

const (
	sessionA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	sessionB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

func TestClassifySessions(t *testing.T) {
	upperA := "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"
	cases := []struct {
		name           string
		items          []sessionItem
		rows           []sessionRow
		newFormat      bool
		missing, stale []string
	}{
		{"no row is missing", []sessionItem{{sessionA, 100}}, nil, true, []string{sessionA}, []string{}},
		{"any row makes a session present", []sessionItem{{sessionA, 100}}, []sessionRow{{sessionA, 100}}, true, []string{}, []string{}},
		{"stale only when the file outgrew the largest size",
			[]sessionItem{{sessionA, 150}, {sessionB, 90}}, []sessionRow{{sessionA, 100}, {sessionB, 100}}, true, []string{}, []string{sessionA}},
		{"legacy clients are never stale", []sessionItem{{sessionA, 0}}, []sessionRow{{sessionA, 100}}, false, []string{}, []string{}},
		{"server ids match case-insensitively", []sessionItem{{sessionA, 1}}, []sessionRow{{upperA, 1}}, true, []string{}, []string{}},
		{"an upper-case client id matches", []sessionItem{{upperA, 1}}, []sessionRow{{sessionA, 1}}, true, []string{}, []string{}},
	}
	for _, c := range cases {
		missing, stale := classifySessions(c.items, c.rows, c.newFormat)
		if !reflect.DeepEqual(missing, c.missing) || !reflect.DeepEqual(stale, c.stale) {
			t.Errorf("%s: missing %v stale %v", c.name, missing, stale)
		}
	}
}

func TestSliceUTF16(t *testing.T) {
	cases := map[string]string{
		"0123456789": "01234567",
		"short":      "short",
		"0123456😀x":  "0123456\uFFFD",
		"012345😀x":   "012345😀",
		"":           "",
		"日本語日本語日本語":  "日本語日本語日本",
	}
	for in, want := range cases {
		if got := sliceUTF16(in, 8); got != want {
			t.Errorf("sliceUTF16(%q) = %q, want %q", in, got, want)
		}
	}
}

func syncRoutes(t *testing.T) (*Routes, pgxmock.PgxPoolIface) {
	rt, mock, _ := newRoutes(t, map[string]string{"conversation_retention_days": "14"})
	rt.Namespaces = &permissions.Namespaces{DB: mock}
	rt.Location = time.UTC
	return rt, mock
}

// The 400s Node wrote directly, in its order, before any query.
func TestMemorySyncRefusals(t *testing.T) {
	rt, mock := syncRoutes(t)
	file := func(fields string) string {
		return `{"memory":{"files":[{"filename":"a.md","content":"x","mtime":"2026-11-06T00:00:00Z"},` + fields + `]}}`
	}
	cases := []struct {
		name, body, agent string
		status            int
		message           string
	}{
		{"admin", `{}`, "", 401, "Agent session required"},
		{"files missing", `{"memory":{}}`, "agent-a", 400, "memory.files must be an array of {filename, content, mtime}"},
		{"memory true", `{"memory":true}`, "agent-a", 400, "memory.files must be an array of {filename, content, mtime}"},
		{"memory array", `{"memory":[1]}`, "agent-a", 400, "memory.files must be an array of {filename, content, mtime}"},
		{"files object", `{"memory":{"files":{}}}`, "agent-a", 400, "memory.files must be an array of {filename, content, mtime}"},
		{"file null", file(`null`), "agent-a", 400, "memory.files[1] is not an object"},
		{"file string", file(`"a.md"`), "agent-a", 400, "memory.files[1] is not an object"},
		{"file array", file(`[]`), "agent-a", 400, "memory.files[1].filename is invalid: must be a safe basename (no slashes, no .., no leading dot)"},
		{"nested name", file(`{"filename":"a/b.md"}`), "agent-a", 400, "memory.files[1].filename is invalid: must be a safe basename (no slashes, no .., no leading dot)"},
		{"dot name", file(`{"filename":".x"}`), "agent-a", 400, "memory.files[1].filename is invalid: must be a safe basename (no slashes, no .., no leading dot)"},
		{"content missing", file(`{"filename":"b.md"}`), "agent-a", 400, "memory.files[1].content must be a string"},
		{"content number", file(`{"filename":"b.md","content":5}`), "agent-a", 400, "memory.files[1].content must be a string"},
		{"mtime missing", file(`{"filename":"b.md","content":""}`), "agent-a", 400, "memory.files[1].mtime must be a valid ISO8601 timestamp"},
		{"mtime zero", file(`{"filename":"b.md","content":"","mtime":0}`), "agent-a", 400, "memory.files[1].mtime must be a valid ISO8601 timestamp"},
		{"mtime garbage", file(`{"filename":"b.md","content":"","mtime":"soon"}`), "agent-a", 400, "memory.files[1].mtime must be a valid ISO8601 timestamp"},
		{"mtime object", file(`{"filename":"b.md","content":"","mtime":{}}`), "agent-a", 400, "memory.files[1].mtime must be a valid ISO8601 timestamp"},
		{"prune without scan age", `{"memory":{"files":[],"prune":true}}`, "agent-a", 400,
			"memory.scan_age_ms must be a non-negative number of milliseconds no greater than 3600000 when memory.prune is true"},
		{"prune with a string age", `{"memory":{"files":[],"prune":true,"scan_age_ms":"5"}}`, "agent-a", 400,
			"memory.scan_age_ms must be a non-negative number of milliseconds no greater than 3600000 when memory.prune is true"},
		{"files checked before prune", `{"memory":{"files":[null],"prune":true}}`, "agent-a", 400, "memory.files[0] is not an object"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		if err := rt.memorySync(rec, request(c.body, c.agent, 11)); err != nil {
			t.Errorf("%s: err %v", c.name, err)
			continue
		}
		code := "BAD_REQUEST"
		if c.status == 401 {
			code = "UNAUTHORIZED"
		}
		if rec.Code != c.status || rec.Body.String() != errorBody(code, c.message) {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestMemorySyncThrownErrors(t *testing.T) {
	rt, mock := syncRoutes(t)
	cases := []struct{ name, body, want string }{
		{"no body", ``, "Cannot read properties of undefined (reading 'memory')"},
		{"mtime without a primitive", `{"memory":{"files":[{"filename":"a.md","content":"","mtime":{"toString":1}}]}}`,
			"Cannot convert object to primitive value"},
	}
	for _, c := range cases {
		err := rt.memorySync(httptest.NewRecorder(), request(c.body, "agent-a", 11))
		if err == nil || err.Error() != c.want {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// An empty body, or one with neither half, answers {}.
func TestMemorySyncNothingAsked(t *testing.T) {
	rt, mock := syncRoutes(t)
	for _, body := range []string{`{}`, `[]`, `{"memory":0,"conversations":null}`} {
		rec := httptest.NewRecorder()
		if err := rt.memorySync(rec, request(body, "agent-a", 11)); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 200 || rec.Body.String() != `{}` {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

var listColumns = []string{"id", "slug", "title", "snippet", "content_hash", "created_by", "created_at", "updated_at", "updated_at_exact", "last_accessed"}

func listRow(rows *pgxmock.Rows, id int, slug, updated string) *pgxmock.Rows {
	u := ts(updated)
	exact := u.UTC().Format("2006-01-02T15:04:05.000000Z")
	return rows.AddRow(id, slug, (*string)(nil), "", "hash", (*string)(nil), &u, &u, &exact, (*time.Time)(nil))
}

var readColumns = []string{"id", "namespace", "slug", "title", "content", "created_by", "created_at", "updated_at", "metadata", "extension"}

func expectRead(mock pgxmock.PgxPoolIface, slug string, found bool, title *string, content, updated string) {
	q := mock.ExpectQuery(regexp.QuoteMeta("WHERE d.namespace = $1 AND LOWER(d.slug) = LOWER($2) AND d.deleted_at IS NULL")).
		WithArgs("agent-a", slug)
	if !found {
		q.WillReturnRows(pgxmock.NewRows(readColumns))
		return
	}
	u := ts(updated)
	q.WillReturnRows(pgxmock.NewRows(readColumns).AddRow(1, "agent-a", slug, title, content, (*string)(nil), &u, &u, []byte(nil), (*string)(nil)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE documents SET last_accessed = NOW()")).
		WithArgs("agent-a", slug).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
}

// expectDelete is a conditional delete: "ok" deletes, "changed" finds the
// note rewritten (409), "gone" finds no live note (404).
func expectDelete(mock pgxmock.PgxPoolIface, slug, exact, outcome string) {
	q := mock.ExpectQuery(regexp.QuoteMeta("SET deleted_at = NOW()")).WithArgs("agent-a", slug, exact)
	if outcome == "ok" {
		q.WillReturnRows(pgxmock.NewRows([]string{"id", "content_length"}).AddRow(5, intPtr(3)))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM memory_chunks")).WithArgs("agent-a", slug).
			WillReturnResult(pgxmock.NewResult("DELETE", 1))
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO namespace_usage")).WithArgs("agent-a", -1, -3).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
		return
	}
	q.WillReturnRows(pgxmock.NewRows([]string{"id", "content_length"}))
	live := mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM documents WHERE namespace = $1")).WithArgs("agent-a", slug)
	if outcome == "changed" {
		live.WillReturnRows(pgxmock.NewRows([]string{"?column?"}).AddRow(1))
	} else {
		live.WillReturnRows(pgxmock.NewRows([]string{"?column?"}))
	}
}

func intPtr(n int) *int { return &n }

// Pull, unchanged, prune (deleted, rewritten under the run, already gone),
// a note written after the scan that vanished before its read, a collision,
// skipped slugs, and local names Node read through Object.prototype.
func TestMemorySyncMemory(t *testing.T) {
	rt, mock := syncRoutes(t)
	rt.Now = func() time.Time { return ts("2026-11-06T12:00:00.123Z") }

	rows := pgxmock.NewRows(listColumns)
	listRow(rows, 1, "instructions/memory/late.md", "2026-11-06T12:00:00Z")
	listRow(rows, 2, "instructions/memory/remote-newer.md", "2026-11-06T11:00:00Z")
	listRow(rows, 3, "instructions/memory/same.md", "2026-11-06T10:00:00.5Z")
	listRow(rows, 4, "instructions/memory/retired", "2026-11-05T00:00:03Z")
	listRow(rows, 5, "instructions/memory/rewritten.md", "2026-11-05T00:00:02Z")
	listRow(rows, 6, "instructions/memory/gone.md", "2026-11-05T00:00:01Z")
	listRow(rows, 7, "instructions/memory/nested/x.md", "2026-11-05T00:00:00Z")
	listRow(rows, 8, "Instructions/Memory/case.md", "2026-11-05T00:00:00Z")
	listRow(rows, 9, "instructions/memory/dup.md", "2026-11-04T00:00:00.000900Z")
	listRow(rows, 10, "instructions/memory/dup", "2026-11-04T00:00:00.000100Z")
	listRow(rows, 11, "instructions/memory/.hidden", "2026-11-03T00:00:00Z")
	mock.ExpectQuery(regexp.QuoteMeta("LOWER(d.slug) LIKE LOWER($4)")).
		WithArgs("agent-a", pgxmock.AnyArg(), pgxmock.AnyArg(), "instructions/memory/%").
		WillReturnRows(rows)

	expectRead(mock, "instructions/memory/late.md", false, nil, "", "")
	expectRead(mock, "instructions/memory/remote-newer.md", true, strPtr("Remote"), "R", "2026-11-06T11:00:00Z")
	expectDelete(mock, "instructions/memory/retired", "2026-11-05T00:00:03.000000Z", "ok")
	expectDelete(mock, "instructions/memory/rewritten.md", "2026-11-05T00:00:02.000000Z", "changed")
	expectRead(mock, "instructions/memory/rewritten.md", true, nil, "W", "2026-11-06T11:30:00Z")
	expectDelete(mock, "instructions/memory/gone.md", "2026-11-05T00:00:01.000000Z", "gone")
	expectDelete(mock, "instructions/memory/dup.md", "2026-11-04T00:00:00.000900Z", "ok")

	body := `{"memory":{"prune":true,"scan_age_ms":1000,"files":[
        {"filename":"remote-newer.md","content":"local","mtime":"2026-11-06T10:00:00Z"},
        {"filename":"same.md","content":"x","mtime":"2026-11-06T10:00:00.5009Z"},
        {"filename":"toString","content":"x","mtime":1},
        {"filename":"__proto__","content":"x","mtime":1},
        {"filename":"constructor","content":"x","mtime":true}]}}`
	rec := httptest.NewRecorder()
	if err := rt.memorySync(rec, request(body, "agent-a", 11)); err != nil {
		t.Fatal(err)
	}
	want := `{"memory":{"actions":[` +
		`{"filename":"remote-newer.md","action":"pull","content":"R","title":"Remote","remote_updated_at":"2026-11-06T11:00:00.000Z"},` +
		`{"filename":"same.md","action":"unchanged"},` +
		`{"filename":"retired.md","action":"prune","remote_updated_at":"2026-11-05T00:00:03.000Z"},` +
		`{"filename":"rewritten.md","action":"pull","content":"W","title":null,"remote_updated_at":"2026-11-06T11:30:00.000Z"},` +
		`{"filename":"gone.md","action":"prune","remote_updated_at":"2026-11-05T00:00:01.000Z"},` +
		`{"filename":"dup.md","action":"prune","remote_updated_at":"2026-11-04T00:00:00.000Z"},` +
		`{"filename":"toString","action":"unchanged"},` +
		`{"filename":"constructor","action":"unchanged"}]}}`
	if rec.Code != 200 || rec.Body.String() != want {
		t.Errorf("%d\n got %s\nwant %s", rec.Code, rec.Body, want)
	}
	testutil.WaitMet(t, mock)
}

func expectSessions(mock pgxmock.PgxPoolIface, ids []string, rows *pgxmock.Rows) {
	mock.ExpectQuery(regexp.QuoteMeta("AND metadata->>'session_id' = ANY($2)")).
		WithArgs("agent-a", ids).
		WillReturnRows(rows)
}

func sessionRows(pairs ...any) *pgxmock.Rows {
	rows := pgxmock.NewRows([]string{"session_id", "file_size"})
	for i := 0; i < len(pairs); i += 2 {
		size := int64(pairs[i+1].(int))
		rows.AddRow(pairs[i].(string), &size)
	}
	return rows
}

func TestMemorySyncConversations(t *testing.T) {
	upperB := "BBBBBBBB-BBBB-BBBB-BBBB-BBBBBBBBBBBB"
	cc := "cccccccc-cccc-cccc-cccc-cccccccccccc"
	cases := []struct {
		name   string
		body   string
		ids    []string
		rows   *pgxmock.Rows
		want   string
		config map[string]string
	}{
		{"retention only", `{"conversations":{}}`, nil, nil, `{"conversations":{"retention_days":14}}`, nil},
		{"retention default", `{"conversations":5}`, nil, nil, `{"conversations":{"retention_days":30}}`, map[string]string{"conversation_retention_days": "x"}},
		{"sized sessions",
			`{"conversations":{"sessions":[{"id":"` + sessionA + `","file_size":150},{"id":"` + upperB + `","file_size":"90"},` +
				`{"id":"` + cc + `","file_size":-1},{"id":"` + sessionA + `"},{"id":"bad"},null,5,{"id":7}]}}`,
			[]string{sessionA, sessionB, cc}, sessionRows(sessionA, 100, sessionB, 100),
			`{"conversations":{"retention_days":14,"missing":["` + cc + `"],"stale":["` + sessionA + `"]}}`, nil},
		{"legacy ids", `{"conversations":{"session_ids":["` + sessionA + `","` + sessionB + `",5]}}`,
			[]string{sessionA, sessionB}, sessionRows(sessionA, 1),
			`{"conversations":{"retention_days":14,"missing":["` + sessionB + `"]}}`, nil},
		{"sessions win over legacy ids", `{"conversations":{"sessions":[{"id":"` + cc + `"}],"session_ids":["` + sessionA + `"]}}`,
			[]string{cc}, sessionRows(),
			`{"conversations":{"retention_days":14,"missing":["` + cc + `"]}}`, nil},
		{"no valid ids still queries", `{"conversations":{"sessions":[{"id":"x"}]}}`,
			[]string{}, sessionRows(), `{"conversations":{"retention_days":14,"missing":[]}}`, nil},
		{"an empty sessions array is not the new format", `{"conversations":{"sessions":[],"session_ids":[]}}`,
			nil, nil, `{"conversations":{"retention_days":14}}`, nil},
	}
	for _, c := range cases {
		rt, mock := syncRoutes(t)
		for k, v := range c.config {
			rt.Config.Set(k, v)
		}
		if c.rows != nil {
			expectSessions(mock, c.ids, c.rows)
		}
		rec := httptest.NewRecorder()
		if err := rt.memorySync(rec, request(c.body, "agent-a", 11)); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 200 || rec.Body.String() != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, rec.Body, c.want)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestMemorySyncTooManySessions(t *testing.T) {
	rt, mock := syncRoutes(t)
	ids := make([]string, 1001)
	for i := range ids {
		ids[i] = sessionA
	}
	raw, _ := json.Marshal(ids)
	rec := httptest.NewRecorder()
	body := `{"conversations":{"session_ids":` + string(raw) + `,"uploads":[{"content":"c"}]}}`
	if err := rt.memorySync(rec, request(body, "agent-a", 11)); err != nil {
		t.Fatal(err)
	}
	want := `{"conversations":{"retention_days":14,"error":"Too many session IDs (max 1000)","uploaded":0,` +
		`"upload_errors":[{"session_id":"unknown","error":"Missing required fields: content, session_id, date"}]}}`
	if rec.Body.String() != want {
		t.Errorf("got %s", rec.Body)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func expectConversationError(mock pgxmock.PgxPoolIface, action, message string) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, name FROM actors WHERE name = $1")).WithArgs("agent-a").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(11, "agent-a"))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO error_log")).
		WithArgs("agent", action, intPtr(11), (*string)(nil), (*string)(nil), message, pgxmock.AnyArg(), (*int)(nil)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
}

func TestMemorySyncUploadErrors(t *testing.T) {
	cases := []struct {
		name, uploads, want string
		logged              string
	}{
		{"missing fields keep a truthy session id as sent",
			`[{"session_id":5,"date":"d"},"x",[],{"content":"c","session_id":"","date":"d"}]`,
			`"uploaded":0,"upload_errors":[{"session_id":5,"error":"Missing required fields: content, session_id, date"},` +
				`{"session_id":"unknown","error":"Missing required fields: content, session_id, date"},` +
				`{"session_id":"unknown","error":"Missing required fields: content, session_id, date"},` +
				`{"session_id":"unknown","error":"Missing required fields: content, session_id, date"}]`, ""},
		{"a date that is not a string", `[{"content":"c","session_id":"s","date":20261106}]`,
			`"uploaded":0,"upload_errors":[{"session_id":"s","error":"date must be a string"}]`, "date must be a string"},
		{"a session id that is not a string", `[{"content":"c","session_id":["s"],"date":"d"}]`,
			`"uploaded":0,"upload_errors":[{"session_id":["s"],"error":"session_id must be a string"}]`, "session_id must be a string"},
		{"content that is not a string", `[{"content":{"a":1},"session_id":"s","date":"d"}]`,
			`"uploaded":0,"upload_errors":[{"session_id":"s","error":"content must be a string"}]`, "content must be a string"},
	}
	for _, c := range cases {
		rt, mock := syncRoutes(t)
		if c.logged != "" {
			expectConversationError(mock, "conversation-upload", c.logged)
		}
		rec := httptest.NewRecorder()
		if err := rt.memorySync(rec, request(`{"conversations":{"uploads":`+c.uploads+`}}`, "agent-a", 11)); err != nil {
			t.Fatal(err)
		}
		want := `{"conversations":{"retention_days":14,` + c.want + `}}`
		if rec.Body.String() != want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, rec.Body, want)
		}
		testutil.WaitMet(t, mock)
	}
}

// Errors that ended Node's conversation try block: a null upload (its catch
// threw again), Number() of an object with its own toString, and a failed
// query. The members set before the error stay.
func TestMemorySyncConversationFailures(t *testing.T) {
	cases := []struct {
		name, body, want, logged string
		query                    bool
	}{
		{"null upload", `{"conversations":{"uploads":[{"content":"c"},null]}}`,
			`{"conversations":{"retention_days":14,"error":"Conversation sync failed: Cannot read properties of null (reading 'session_id')"}}`,
			"Cannot read properties of null (reading 'session_id')", false},
		{"file size without a primitive", `{"conversations":{"sessions":[{"id":"` + sessionA + `","file_size":{"toString":1}}]}}`,
			`{"conversations":{"retention_days":14,"error":"Conversation sync failed: Cannot convert object to primitive value"}}`,
			"Cannot convert object to primitive value", false},
		{"query failure", `{"conversations":{"session_ids":["` + sessionA + `"]}}`,
			`{"conversations":{"retention_days":14,"error":"Conversation sync failed: boom"}}`, "boom", true},
	}
	for _, c := range cases {
		rt, mock := syncRoutes(t)
		if c.query {
			mock.ExpectQuery(regexp.QuoteMeta("AND metadata->>'session_id' = ANY($2)")).
				WithArgs("agent-a", []string{sessionA}).WillReturnError(errorString("boom"))
		}
		expectConversationError(mock, "conversation-sync", c.logged)
		rec := httptest.NewRecorder()
		if err := rt.memorySync(rec, request(c.body, "agent-a", 11)); err != nil {
			t.Fatal(err)
		}
		if rec.Body.String() != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, rec.Body, c.want)
		}
		testutil.WaitMet(t, mock)
	}

	// A missing config key fails the half before anything is set.
	rt, mock := syncRoutes(t)
	rt.Config = config.NewStore()
	expectConversationError(mock, "conversation-sync", `Config key "conversation_retention_days" not found. Was config.init() called?`)
	rec := httptest.NewRecorder()
	if err := rt.memorySync(rec, request(`{"conversations":{}}`, "agent-a", 11)); err != nil {
		t.Fatal(err)
	}
	want := `{"conversations":{"error":"Conversation sync failed: Config key \"conversation_retention_days\" not found. Was config.init() called?"}}`
	if rec.Body.String() != want {
		t.Errorf("missing key: %s", rec.Body)
	}
	testutil.WaitMet(t, mock)
}

type errorString string

func (e errorString) Error() string { return string(e) }
