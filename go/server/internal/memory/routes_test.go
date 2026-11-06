package memory

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// statusFiles is the fixture for an all-namespace status: one file in
// each of the caller's own namespace, a granted one, a namespace named
// after the actor type, and one the caller cannot read.
var statusFiles = [][2]string{
	{"agent", "type-named.md"},
	{"agent-b", "own.md"},
	{"private-ns", "secret.md"},
	{"shared", "notes.md"},
}

// ingestStatusAll runs /memory/ingest/status with no namespace as agent-b
// (actor 7) with the given grants and files, and returns the answer.
func ingestStatusAll(t *testing.T, grants [][4]any, files [][2]string) string {
	t.Helper()
	mock := testutil.NewMock(t)
	grantRows := pgxmock.NewRows([]string{"namespace", "can_read", "can_write", "can_delete"})
	for _, g := range grants {
		grantRows.AddRow(g[0], g[1], g[2], g[3])
	}
	mock.ExpectQuery("FROM namespace_permissions WHERE actor_id").WithArgs(7).WillReturnRows(grantRows)
	fileRows := pgxmock.NewRows([]string{"namespace", "source_file", "ingested_at", "chunk_count"})
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for _, f := range files {
		fileRows.AddRow(f[0], f[1], at, int64(1))
	}
	mock.ExpectQuery("FROM memory_chunks").WillReturnRows(fileRows)

	rt := &Routes{Service: &Service{DB: mock}, Namespaces: &permissions.Namespaces{DB: mock}}
	r := httptest.NewRequest("POST", "/v1/memory/ingest/status", nil)
	r = httpapi.WithStateValue(r, &httpapi.State{AuthenticatedAgent: "agent-b", ActorID: 7, Body: json.RawMessage(`{}`)})
	rec := httptest.NewRecorder()
	if err := rt.ingestStatus(rec, r); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	return rec.Body.String()
}

func statusBody(files ...[2]string) string {
	out := `{"files":[`
	for i, f := range files {
		if i > 0 {
			out += ","
		}
		out += `{"namespace":"` + f[0] + `","source_file":"` + f[1] + `","ingested_at":"2026-11-06T12:00:00.000Z","chunk_count":"1"}`
	}
	return out + `]}`
}

// The caller reads its grants and its own namespace (by name, with no
// grant), not a namespace named after its actor type.
func TestIngestStatusAllReadsOwnNamespace(t *testing.T) {
	got := ingestStatusAll(t, [][4]any{{"shared", true, false, false}}, statusFiles)
	want := statusBody(statusFiles[1], statusFiles[3])
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// A wildcard reader gets every file, unfiltered, and [] when there are
// none.
func TestIngestStatusAllWildcardReader(t *testing.T) {
	wildcard := [][4]any{{"/", true, false, false}}
	got := ingestStatusAll(t, wildcard, statusFiles)
	if want := statusBody(statusFiles...); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if got := ingestStatusAll(t, wildcard, nil); got != `{"files":[]}` {
		t.Errorf("empty: %s", got)
	}
}
