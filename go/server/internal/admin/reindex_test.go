package admin

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/memory"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

type enrichCall struct{ namespace, slug, title, content, metadata string }

type fakeEnricher struct {
	mu    sync.Mutex
	calls []enrichCall
}

func (f *fakeEnricher) Enrich(ctx context.Context, namespace, slug, title, content string, metadata []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	meta := "<nil>"
	if metadata != nil {
		meta = string(metadata)
	}
	f.calls = append(f.calls, enrichCall{namespace, slug, title, content, meta})
	return nil
}

func (f *fakeEnricher) snapshot() []enrichCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]enrichCall(nil), f.calls...)
}

func awaitReindex(t *testing.T, h http.Handler) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := serve(h, asUser(1, "/v1/admin/notes/reindex-status", `{}`))
		if !strings.Contains(rec.Body.String(), `"running":true`) || time.Now().After(deadline) {
			return rec.Body.String()
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Node read note_enrichment_enabled with config.get, which threw on a
// missing key outside the per-note try: the reindex fails after the chunks
// are deleted.
func TestReindexWithoutTheEnrichmentKey(t *testing.T) {
	h, mock := newContentWith(t, func(rt *ContentRoutes) { rt.Config = config.NewStore() })
	grants(mock, 1, "notes", "write", "notes", "read")
	expectReindexRun(mock)
	check(t, "start", serve(h, asUser(1, "/v1/admin/notes/reindex", `{}`)), 200, `{"started":true}`)
	testutil.WaitMet(t, mock)
	want := `{"running":false,"current":0,"total":0,"chunks_created":0,"errors_count":0,"result":{"error":"Config key \"note_enrichment_enabled\" not found. Was config.init() called?"}}`
	if got := awaitReindex(t, h); got != want {
		t.Fatalf("status %s", got)
	}
}

// With enrichment on, each indexed note is enriched with its metadata as
// Node parsed it and a NULL title as the text "null"; metadata JSON.parse
// cannot read is that note's error.
func TestReindexEnrichesEachNote(t *testing.T) {
	enricher := &fakeEnricher{}
	h, mock := newContentWith(t, func(rt *ContentRoutes) {
		store := config.NewStore()
		store.Set("note_enrichment_enabled", "true")
		rt.Config = store
		// context/ notes are not chunked, so the ingest touches nothing.
		rt.Memory = &memory.Service{}
		rt.Notes.Enricher = enricher
	})
	grants(mock, 1, "notes", "write", "notes", "read")
	mock.ExpectExec("DELETE FROM memory_chunks").WillReturnResult(pgxmock.NewResult("DELETE", 0))
	untitled := (*string)(nil)
	titled := "B"
	other := "C"
	mock.ExpectQuery("SELECT namespace, slug, content, title, metadata FROM documents").
		WillReturnRows(pgxmock.NewRows([]string{"namespace", "slug", "content", "title", "metadata"}).
			AddRow("agent-a", "context/a", "one", untitled, []byte(`{"_enriched_at": "2026-01-01T00:00:00.000Z"}`)).
			AddRow("agent-a", "context/b", "two", &titled, []byte(`"not json"`)).
			AddRow("agent-a", "context/c", "three", &other, []byte(`"{\"k\": 1}"`)).
			AddRow("agent-b", "context/d", "four", &other, []byte(nil)))
	check(t, "start", serve(h, asUser(1, "/v1/admin/notes/reindex", `{}`)), 200, `{"started":true}`)
	testutil.WaitMet(t, mock)
	status := awaitReindex(t, h)
	if !strings.Contains(status, `"result":{"chunks_deleted":0,"docs_indexed":3,"chunks_created":0,"errors":[{"namespace":"agent-a","slug":"context/b","error":`) {
		t.Fatalf("status %s", status)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(enricher.snapshot()) < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	want := []enrichCall{
		{"agent-a", "context/a", "null", "one", `{"_enriched_at": "2026-01-01T00:00:00.000Z"}`},
		{"agent-a", "context/c", "C", "three", `{"k": 1}`},
		{"agent-b", "context/d", "C", "four", "<nil>"},
	}
	got := enricher.snapshot()
	if len(got) != len(want) {
		t.Fatalf("calls %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d: %+v, want %+v", i, got[i], want[i])
		}
	}
}
