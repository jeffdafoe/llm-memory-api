package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/notes"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/pgjson"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/visibility"
)

// The ports of node/api/src/routes/admin-notes-batch.test.js (LLM-829): the
// batched /admin/notes/list and the shares/list `all` filter. As in the
// Node test, wendy (1) reads every namespace through a '/' grant and carol
// (2) reads "alpha" plus her own namespace.

func newContent(t *testing.T) (http.Handler, pgxmock.PgxPoolIface) {
	mock := testutil.NewMock(t)
	// The list queries share one statement; their order is what the
	// tests check.
	mock.MatchExpectationsInOrder(true)
	cache := actors.NewCache(mock)
	log := logger.New(mock, cache, "")
	adm := &permissions.Admin{DB: mock}
	rt := &ContentRoutes{
		Common: &Common{
			DB: mock, Log: log, Router: &httpapi.Router{Log: log}, Admin: adm, Actors: cache,
			Visibility: &visibility.Visibility{DB: mock, Admin: adm},
		},
		Config:     config.NewStore(),
		Notes:      &notes.Service{DB: mock},
		Namespaces: &permissions.Namespaces{DB: mock},
	}
	mux := NewMux()
	rt.Register(mux)
	return Handler(mux), mock
}

var batchUsers = map[int]string{1: "wendy", 2: "carol"}

func asUser(actorID int, path, body string) *http.Request {
	r := httptest.NewRequest("POST", path, nil)
	st := &httpapi.State{ActorID: actorID, AuthenticatedUser: &httpapi.User{ID: actorID, Username: batchUsers[actorID]}}
	st.Body = json.RawMessage(body)
	return httpapi.WithStateValue(r, st)
}

// batchGrants is the lookups the Node test's pool answered: the notes:read
// admin grant for requirePerm and the caller's namespace rows.
func batchGrants(mock pgxmock.PgxPoolIface, actorID int) {
	grants(mock, actorID, "notes", "read")
	rows := pgxmock.NewRows([]string{"namespace", "can_read", "can_write", "can_delete"})
	if actorID == 1 {
		rows.AddRow("/", true, true, true)
	} else {
		rows.AddRow("alpha", true, false, false)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT namespace, can_read, can_write, can_delete FROM namespace_permissions WHERE actor_id = $1")).
		WithArgs(actorID).WillReturnRows(rows)
}

var listCols = []string{"id", "slug", "title", "snippet", "content_hash", "created_by", "created_at", "updated_at", "updated_at_exact", "last_accessed"}

// expectList is one listNotes query for namespace with the given limit.
func expectList(mock pgxmock.PgxPoolIface, namespace string, limit string) {
	mock.ExpectQuery("FROM documents d").
		WithArgs(namespace, jsutil.Number(limit), 0).
		WillReturnRows(pgxmock.NewRows(listCols))
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestBatchListInTheOrderAsked(t *testing.T) {
	h, mock := newContent(t)
	batchGrants(mock, 1)
	expectList(mock, "beta", "500")
	expectList(mock, "alpha", "500")
	rec := serve(h, asUser(1, "/v1/admin/notes/list", `{"namespaces":["beta","alpha"],"limit":500}`))
	check(t, "batch", rec, 200, `{"results":[{"namespace":"beta","notes":[]},{"namespace":"alpha","notes":[]}]}`)
	testutil.WaitMet(t, mock)
}

func TestBatchListOwnNamespaceAndDefaultLimit(t *testing.T) {
	h, mock := newContent(t)
	batchGrants(mock, 2)
	expectList(mock, "alpha", "200")
	expectList(mock, "carol", "200")
	rec := serve(h, asUser(2, "/v1/admin/notes/list", `{"namespaces":["alpha","carol"]}`))
	check(t, "own", rec, 200, `{"results":[{"namespace":"alpha","notes":[]},{"namespace":"carol","notes":[]}]}`)
	testutil.WaitMet(t, mock)
}

// One unreadable namespace fails the batch before any namespace is listed:
// no list query is expected.
func TestBatchListRefusedBeforeAnyRead(t *testing.T) {
	h, mock := newContent(t)
	batchGrants(mock, 2)
	expectErrorLog(mock)
	rec := serve(h, asUser(2, "/v1/admin/notes/list", `{"namespaces":["alpha","beta"]}`))
	if rec.Code != 403 {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	testutil.WaitMet(t, mock)
}

func TestBatchListMalformed(t *testing.T) {
	names := make([]string, 501)
	for i := range names {
		names[i] = fmt.Sprintf(`"ns%d"`, i)
	}
	for _, namespaces := range []string{`"alpha"`, `[""]`, `["alpha",7]`, `null`, "[" + strings.Join(names, ",") + "]"} {
		h, mock := newContent(t)
		grants(mock, 1, "notes", "read")
		rec := serve(h, asUser(1, "/v1/admin/notes/list", `{"namespaces":`+namespaces+`}`))
		if rec.Code != 400 {
			t.Errorf("%.40s: status %d", namespaces, rec.Code)
		}
		testutil.WaitMet(t, mock)
	}
}

func TestBatchListReservedNamespace(t *testing.T) {
	h, mock := newContent(t)
	batchGrants(mock, 1)
	expectErrorLog(mock)
	rec := serve(h, asUser(1, "/v1/admin/notes/list", `{"namespaces":["alpha","*"]}`))
	if rec.Code != 400 {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	testutil.WaitMet(t, mock)
}

func TestSingleNamespaceListUnchanged(t *testing.T) {
	h, mock := newContent(t)
	batchGrants(mock, 2)
	expectList(mock, "alpha", "200")
	rec := serve(h, asUser(2, "/v1/admin/notes/list", `{"namespace":"alpha"}`))
	check(t, "single", rec, 200, `{"notes":[]}`)
	testutil.WaitMet(t, mock)

	h, mock = newContent(t)
	batchGrants(mock, 2)
	expectErrorLog(mock)
	rec = serve(h, asUser(2, "/v1/admin/notes/list", `{"namespace":"beta"}`))
	if rec.Code != 403 {
		t.Errorf("refused: status %d", rec.Code)
	}
	testutil.WaitMet(t, mock)
}

// The shares/list `all` filter (filterRows over the readable list).
func TestAllSharesFilter(t *testing.T) {
	share := func(ns string) pgjson.Row {
		v, _ := json.Marshal(ns)
		return pgjson.Row{{Name: "owner_namespace", Value: v}}
	}
	rows := []pgjson.Row{share("alpha"), share("beta"), share("carol")}
	got, _ := json.Marshal(filterRows(rows, "owner_namespace", []string{"alpha", "carol"}))
	if string(got) != `[{"owner_namespace":"alpha"},{"owner_namespace":"carol"}]` {
		t.Errorf("limited: %s", got)
	}
	got, _ = json.Marshal(filterRows(rows, "owner_namespace", nil))
	if string(got) != `[]` {
		t.Errorf("none readable: %s", got)
	}
}

func TestJSGroupsKeyOrder(t *testing.T) {
	var g jsGroups
	for _, k := range []string{"zeta", "10", "alpha", "2", "01", "4294967295"} {
		v, _ := json.Marshal(k)
		g.add(k, pgjson.Row{{Name: "k", Value: v}})
	}
	got, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"2":[{"k":"2"}],"10":[{"k":"10"}],"zeta":[{"k":"zeta"}],"alpha":[{"k":"alpha"}],"01":[{"k":"01"}],"4294967295":[{"k":"4294967295"}]}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestReindexStatusAndClear(t *testing.T) {
	h, mock := newContent(t)
	grants(mock, 1, "notes", "read", "notes", "write")
	rec := serve(h, asUser(1, "/v1/admin/notes/reindex-status", `{}`))
	check(t, "idle", rec, 200, `{"running":false}`)
	rec = serve(h, asUser(1, "/v1/admin/notes/reindex-clear", `{}`))
	check(t, "clear", rec, 200, `{"ok":true}`)
	testutil.WaitMet(t, mock)
}

// expectErrorLog is the error_log row Router.Route writes for a thrown 4xx.
func expectErrorLog(mock pgxmock.PgxPoolIface) {
	args := make([]any, 8)
	for i := range args {
		args[i] = pgxmock.AnyArg()
	}
	mock.ExpectExec("INSERT INTO error_log").WithArgs(args...).WillReturnResult(pgxmock.NewResult("INSERT", 1))
}
