package memory

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// documentsAndChunksSchema is the documents columns IngestDocument checks,
// plus memory_chunks.
const documentsAndChunksSchema = chunksSchema + `;
CREATE TABLE documents (
	id serial PRIMARY KEY,
	namespace text NOT NULL,
	slug text NOT NULL,
	content text NOT NULL,
	deleted_at timestamptz
)`

// duringEmbed runs its hook inside the embedding call, then answers like
// fixedEmbedder. The embedding call is the slow step between a background
// re-index starting and its chunks being written, so a hook here is a
// delete, move or edit landing in that window, on every run.
type duringEmbed struct {
	hook func()
}

func (e duringEmbed) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	e.hook()
	return fixedEmbedder{}.Embed(ctx, texts)
}

func seedDocument(t *testing.T, pool *pgxpool.Pool, slug, content string) int {
	t.Helper()
	var id int
	if err := pool.QueryRow(context.Background(),
		"INSERT INTO documents (namespace, slug, content) VALUES ('ns', $1, $2) RETURNING id", slug, content).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func TestIngestDocumentWritesALiveNote(t *testing.T) {
	pool := testutil.ScratchDB(t, documentsAndChunksSchema)
	content := "# Note\n\nlive text"
	id := seedDocument(t, pool, "notes/doc", content)
	s := &Service{DB: pool, Embedder: fixedEmbedder{}}

	res, err := s.IngestDocument(context.Background(), id, "ns", "notes/doc", content)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped || res.ChunksCreated != 1 || len(storedChunks(t, s, "ns", "notes/doc")) != 1 {
		t.Errorf("got %+v and %d stored chunks, want 1 chunk written", res, len(storedChunks(t, s, "ns", "notes/doc")))
	}
}

// The delete lands while the re-index is embedding: after any check the
// re-index could make up front, before it writes. It must write nothing.
func TestIngestDocumentSkipsANoteDeletedMidIngest(t *testing.T) {
	pool := testutil.ScratchDB(t, documentsAndChunksSchema)
	content := "# Note\n\nabout to be deleted"
	id := seedDocument(t, pool, "notes/doc", content)
	s := &Service{DB: pool, Embedder: duringEmbed{hook: func() {
		exec(t, pool, "UPDATE documents SET deleted_at = now() WHERE id = $1", id)
	}}}

	res, err := s.IngestDocument(context.Background(), id, "ns", "notes/doc", content)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped {
		t.Errorf("got %+v, want Skipped", res)
	}
	if got := storedChunks(t, s, "ns", "notes/doc"); len(got) != 0 {
		t.Errorf("a deleted note got chunks back: %q", got)
	}
}

// A newer edit lands mid-ingest: this re-index holds the old text and must
// not overwrite the chunks the newer edit's own re-index writes.
func TestIngestDocumentSkipsANoteEditedMidIngest(t *testing.T) {
	pool := testutil.ScratchDB(t, documentsAndChunksSchema)
	old := "# Note\n\nold text"
	id := seedDocument(t, pool, "notes/doc", old)
	s := &Service{DB: pool, Embedder: duringEmbed{hook: func() {
		exec(t, pool, "UPDATE documents SET content = 'newer text' WHERE id = $1", id)
	}}}

	res, err := s.IngestDocument(context.Background(), id, "ns", "notes/doc", old)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped || len(storedChunks(t, s, "ns", "notes/doc")) != 0 {
		t.Errorf("got %+v, want the stale re-index skipped", res)
	}
}

// A move lands mid-ingest: the chunks go to the note's current slug, not
// to the slug it had when the re-index started.
func TestIngestDocumentFollowsANoteMovedMidIngest(t *testing.T) {
	pool := testutil.ScratchDB(t, documentsAndChunksSchema)
	content := "# Note\n\nmoving text"
	id := seedDocument(t, pool, "notes/doc", content)
	s := &Service{DB: pool, Embedder: duringEmbed{hook: func() {
		exec(t, pool, "UPDATE documents SET slug = 'notes/moved' WHERE id = $1", id)
	}}}

	res, err := s.IngestDocument(context.Background(), id, "ns", "notes/doc", content)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped || res.SourceFile != "notes/moved" {
		t.Errorf("got %+v, want chunks at notes/moved", res)
	}
	if old, moved := storedChunks(t, s, "ns", "notes/doc"), storedChunks(t, s, "ns", "notes/moved"); len(old) != 0 || len(moved) != 1 {
		t.Errorf("%d chunks at the old slug and %d at the new one, want 0 and 1", len(old), len(moved))
	}
}
