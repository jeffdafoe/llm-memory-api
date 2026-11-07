package notes

import (
	"context"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/memory"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// enrichProbe records the context and metadata the save hands the enricher.
type enrichProbe struct {
	// release holds the probe until the test has cancelled the request.
	release     chan struct{}
	done        chan struct{}
	hasDeadline bool
	cancelled   bool
	metadata    string
}

func (p *enrichProbe) Enrich(ctx context.Context, namespace, slug, title, content string, metadata []byte) error {
	<-p.release
	_, p.hasDeadline = ctx.Deadline()
	p.cancelled = ctx.Err() != nil
	p.metadata = string(metadata)
	close(p.done)
	return nil
}

func newProbe() *enrichProbe {
	return &enrichProbe{release: make(chan struct{}), done: make(chan struct{})}
}

// upsertSaving answers an upsert of agent-a/context/x over a live note of
// the same length (no quota check) whose stored metadata is meta.
func upsertSaving(t *testing.T, meta string, enricher Enricher) (*Service, pgxmock.PgxPoolIface) {
	t.Helper()
	mock := testutil.NewMock(t)
	mock.ExpectQuery("AS content_length FROM documents").WithArgs("agent-a", "context/x").
		WillReturnRows(pgxmock.NewRows([]string{"id", "content_length"}).AddRow(7, func() *int { n := 4; return &n }()))
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery("UPDATE documents").WithArgs("T", "body", 7, "context").
		WillReturnRows(pgxmock.NewRows([]string{"id", "namespace", "slug", "title", "created_by_actor_id", "created_at", "updated_at", "metadata", "extension"}).
			AddRow(7, "agent-a", "context/x", "T", (*int)(nil), &now, &now, []byte(meta), (*string)(nil)))
	mock.ExpectExec("INSERT INTO namespace_usage").WithArgs("agent-a", 0, 0).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	store := config.NewStore()
	store.Set("note_maximum_size", "512000")
	// context/ notes are not chunked, so the ingest touches nothing.
	return &Service{DB: mock, Config: store, Memory: &memory.Service{}, Enricher: enricher}, mock
}

// The enrichment outlives the request and has no time bound of its own:
// the provider call it makes has its own timeout, past the 30 s that
// db.Background allows.
func TestSaveEnrichesUnbounded(t *testing.T) {
	probe := newProbe()
	s, mock := upsertSaving(t, `{"a": 1}`, probe)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := s.Save(ctx, SaveInput{Namespace: "agent-a", Slug: "context/x", Title: "T", Content: "body", Upsert: true}); err != nil {
		t.Fatal(err)
	}
	cancel()
	close(probe.release)
	select {
	case <-probe.done:
	case <-time.After(5 * time.Second):
		t.Fatal("enrichment never ran")
	}
	if probe.hasDeadline || probe.cancelled {
		t.Errorf("enrichment context: deadline %v, cancelled %v", probe.hasDeadline, probe.cancelled)
	}
	if probe.metadata != `{"a": 1}` {
		t.Errorf("metadata %s", probe.metadata)
	}
	testutil.WaitMet(t, mock)
}

// An upsert keeps the stored metadata; Node parsed it in saveNote, so a
// JSON string holding no JSON, or a number past the float64 range, fails
// the save after the row is written.
func TestSaveFailsOnStoredMetadataNodeCannotParse(t *testing.T) {
	for _, meta := range []string{`"not json"`, `1e400`} {
		probe := newProbe()
		close(probe.release)
		s, mock := upsertSaving(t, meta, probe)
		if _, err := s.Save(context.Background(), SaveInput{Namespace: "agent-a", Slug: "context/x", Title: "T", Content: "body", Upsert: true}); err == nil {
			t.Errorf("%s: saved", meta)
		}
		testutil.WaitMet(t, mock)
		select {
		case <-probe.done:
			t.Errorf("%s: enriched", meta)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
