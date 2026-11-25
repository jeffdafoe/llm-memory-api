package notes

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/memory"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// These run Edit and Move against a real Postgres: the defect is which rows
// an UPDATE touches, which a mock that matches SQL text cannot show.
// testutil.ScratchDB says when they run.

// deletedCopiesSchema has the columns Read, Edit and Move use, the
// tables their background work writes, and production's one-live-note-per-
// slug index, which a save at a slug being deleted waits on.
const deletedCopiesSchema = `
CREATE TABLE actors (id serial PRIMARY KEY, name text NOT NULL);
CREATE TABLE documents (
	id serial PRIMARY KEY,
	namespace text NOT NULL,
	slug text NOT NULL,
	title text,
	content text NOT NULL,
	created_at timestamptz DEFAULT now(),
	updated_at timestamptz DEFAULT now(),
	deleted_at timestamptz,
	created_by_actor_id integer,
	kind text NOT NULL DEFAULT 'note',
	last_accessed timestamptz,
	metadata jsonb,
	extension text
);
CREATE UNIQUE INDEX documents_namespace_lower_slug_active_key ON documents (namespace, LOWER(slug)) WHERE deleted_at IS NULL;
CREATE TABLE namespace_usage (
	namespace text PRIMARY KEY,
	note_count integer NOT NULL DEFAULT 0,
	total_bytes bigint NOT NULL DEFAULT 0,
	updated_at timestamptz
);
CREATE TABLE memory_chunks (
	id serial PRIMARY KEY,
	namespace text NOT NULL,
	source_file text NOT NULL,
	heading text,
	chunk_text text NOT NULL,
	embedding text
);`

type zeroEmbedder struct{}

func (zeroEmbedder) Embed(_ context.Context, texts []string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i := range texts {
		out[i] = []float64{0}
	}
	return out, nil
}

// seedNoteWithDeletedCopies writes two soft-deleted copies of ns/notes/doc
// (its restore points) and the live note, and returns the deleted ids.
func seedNoteWithDeletedCopies(t *testing.T, pool *pgxpool.Pool) (deletedIDs []int) {
	t.Helper()
	ctx := context.Background()
	for _, content := range []string{"version one", "version two"} {
		var id int
		if err := pool.QueryRow(ctx,
			"INSERT INTO documents (namespace, slug, content, deleted_at) VALUES ('ns', 'notes/doc', $1, now() - interval '1 day') RETURNING id",
			content).Scan(&id); err != nil {
			t.Fatal(err)
		}
		deletedIDs = append(deletedIDs, id)
	}
	if _, err := pool.Exec(ctx,
		"INSERT INTO documents (namespace, slug, content) VALUES ('ns', 'notes/doc', 'version three, live')"); err != nil {
		t.Fatal(err)
	}
	return deletedIDs
}

func newDeletedCopiesService(pool *pgxpool.Pool) *Service {
	cfg := config.NewStore()
	cfg.Set("note_maximum_size", "512000")
	return &Service{DB: pool, Config: cfg, Memory: &memory.Service{DB: pool, Config: cfg, Embedder: zeroEmbedder{}}}
}

type deletedRow struct {
	slug, content string
}

func readRow(t *testing.T, pool *pgxpool.Pool, id int) deletedRow {
	t.Helper()
	var r deletedRow
	if err := pool.QueryRow(context.Background(),
		"SELECT slug, content FROM documents WHERE id = $1", id).Scan(&r.slug, &r.content); err != nil {
		t.Fatal(err)
	}
	return r
}

// An edit changes only the live note. It used to write the new content into
// every soft-deleted copy of the slug too, so a restore gave back today's
// note instead of the old version.
func TestEditLeavesDeletedCopiesAlone(t *testing.T) {
	pool := testutil.ScratchDB(t, deletedCopiesSchema)
	deleted := seedNoteWithDeletedCopies(t, pool)
	s := newDeletedCopiesService(pool)

	newString := "VERSION THREE"
	out, err := s.Edit(context.Background(), EditInput{Namespace: "ns", Slug: "Notes/Doc", OldString: "version three", NewString: &newString})
	if err != nil {
		t.Fatal(err)
	}
	if out.changedNote == nil {
		t.Fatal("the edit returned no note")
	}

	live, err := s.Read(context.Background(), "ns", "notes/doc")
	if err != nil {
		t.Fatal(err)
	}
	if live.Content != "VERSION THREE, live" {
		t.Errorf("live content = %q", live.Content)
	}
	if out.ID != live.ID {
		t.Errorf("the edit answered with row %d, want the live row %d", out.ID, live.ID)
	}
	for i, id := range deleted {
		want := []string{"version one", "version two"}[i]
		if got := readRow(t, pool, id).content; got != want {
			t.Errorf("deleted copy %d content = %q, want %q", id, got, want)
		}
	}
}

// A move renames only the live note; its soft-deleted copies stay at the old
// slug, as move_folder leaves them.
func TestMoveLeavesDeletedCopiesAlone(t *testing.T) {
	pool := testutil.ScratchDB(t, deletedCopiesSchema)
	deleted := seedNoteWithDeletedCopies(t, pool)
	s := newDeletedCopiesService(pool)

	got, err := s.Move(context.Background(), MoveInput{Namespace: "ns", Slug: "notes/doc", NewSlug: "notes/renamed"})
	if err != nil {
		t.Fatal(err)
	}
	moved, ok := got.(changedNote)
	if !ok {
		t.Fatalf("Move returned %T", got)
	}
	live, err := s.Read(context.Background(), "ns", "notes/renamed")
	if err != nil {
		t.Fatal(err)
	}
	if live.Content != "version three, live" || moved.ID != live.ID {
		t.Errorf("moved row %d with content %q, want the live row %d", moved.ID, live.Content, live.ID)
	}
	for _, id := range deleted {
		if r := readRow(t, pool, id); r.slug != "notes/doc" {
			t.Errorf("deleted copy %d moved to %q", id, r.slug)
		}
	}
}
