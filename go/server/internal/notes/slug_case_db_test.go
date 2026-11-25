package notes

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// Slugs match without case and keep the case they were created with: one
// live note per (namespace, LOWER(slug)). These run on a real Postgres,
// because the rule is the unique index and the SQL that must agree with it.

// slugCaseSchema adds what Save's quota check and move_folder's sync
// mappings read to the deleted-copies schema.
const slugCaseSchema = deletedCopiesSchema + `
CREATE TABLE agent_configuration (actor_id integer, storage_quota bigint);
CREATE TABLE note_synchronization (
	id serial PRIMARY KEY,
	actor_id integer NOT NULL,
	namespace text NOT NULL,
	slug text NOT NULL,
	local_path text,
	UNIQUE (actor_id, namespace, slug)
);`

func liveSlugs(t *testing.T, pool *pgxpool.Pool, namespace string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"SELECT slug FROM documents WHERE namespace = $1 AND deleted_at IS NULL", namespace)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	slugs := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		slugs = append(slugs, s)
	}
	// Byte order in Go: ORDER BY would follow the database collation.
	sort.Strings(slugs)
	return slugs
}

func chunkFiles(t *testing.T, pool *pgxpool.Pool, namespace string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"SELECT DISTINCT source_file FROM memory_chunks WHERE namespace = $1", namespace)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	files := []string{}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	sort.Strings(files)
	return files
}

// waitChunks waits for the background re-index to leave exactly the chunk
// files want, all holding text. The text tells a finished re-index from the
// chunks it replaces, which may already sit under the same file name.
func waitChunks(t *testing.T, pool *pgxpool.Pool, namespace string, want []string, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := chunkFiles(t, pool, namespace)
		var stale int
		if err := pool.QueryRow(context.Background(),
			"SELECT COUNT(*) FROM memory_chunks WHERE namespace = $1 AND chunk_text NOT LIKE '%' || $2 || '%'",
			namespace, text).Scan(&stale); err != nil {
			t.Fatal(err)
		}
		if reflect.DeepEqual(got, want) && stale == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("chunk files %v (%d without %q), want %v", got, stale, text, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// syncSlugs is the actor's sync mappings in the namespace, in byte order.
func syncSlugs(t *testing.T, pool *pgxpool.Pool, actorID int, namespace string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"SELECT slug FROM note_synchronization WHERE actor_id = $1 AND namespace = $2", actorID, namespace)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	slugs := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	return slugs
}

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Status != status {
		t.Fatalf("err %v, want status %d", err, status)
	}
}

// A plain save at a case variant of a live slug is a duplicate. With the
// exact-case index it made a second live note, and a read or delete then
// matched both.
func TestSaveAtACaseVariantIsADuplicate(t *testing.T) {
	pool := testutil.ScratchDB(t, slugCaseSchema)
	s := newDeletedCopiesService(pool)
	ctx := context.Background()
	if _, err := s.Save(ctx, SaveInput{Namespace: "ns", Slug: "notes/a", Title: "A", Content: "first"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Save(ctx, SaveInput{Namespace: "ns", Slug: "Notes/A", Title: "A", Content: "second"})
	var dup *DuplicateSlugError
	if !errors.As(err, &dup) {
		t.Fatalf("err %v, want DuplicateSlugError", err)
	}
	if got := liveSlugs(t, pool, "ns"); !reflect.DeepEqual(got, []string{"notes/a"}) {
		t.Fatalf("live %v", got)
	}
}

// An upsert or edit that names the note in another case keeps the stored
// slug, and the chunks go under the stored slug too, not the one asked for:
// IngestDocument re-reads the slug from the row by document id. The share
// search and memory cleanup match chunk files to slugs, so they rely on it.
func TestUpsertAndEditInAnotherCaseIndexUnderTheStoredSlug(t *testing.T) {
	pool := testutil.ScratchDB(t, slugCaseSchema)
	s := newDeletedCopiesService(pool)
	ctx := context.Background()
	if _, err := s.Save(ctx, SaveInput{Namespace: "ns", Slug: "notes/a", Title: "A", Content: "first"}); err != nil {
		t.Fatal(err)
	}
	waitChunks(t, pool, "ns", []string{"notes/a"}, "first")

	doc, err := s.Save(ctx, SaveInput{Namespace: "ns", Slug: "NOTES/A", Title: "A", Content: "second", Upsert: true})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Slug != "notes/a" {
		t.Fatalf("upsert answered slug %q", doc.Slug)
	}
	waitChunks(t, pool, "ns", []string{"notes/a"}, "second")

	replacement := "third"
	if _, err := s.Edit(ctx, EditInput{Namespace: "ns", Slug: "Notes/a", OldString: "second", NewString: &replacement}); err != nil {
		t.Fatal(err)
	}
	waitChunks(t, pool, "ns", []string{"notes/a"}, "third")
	if got := liveSlugs(t, pool, "ns"); !reflect.DeepEqual(got, []string{"notes/a"}) {
		t.Fatalf("live %v", got)
	}
}

// Restoring a deleted note while a live note holds the slug in another case
// is a 409, not a second live note (and not a 500).
func TestRestoreUnderALiveCaseVariantIsAConflict(t *testing.T) {
	pool := testutil.ScratchDB(t, slugCaseSchema)
	s := newDeletedCopiesService(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
        INSERT INTO documents (namespace, slug, content, deleted_at) VALUES ('ns', 'Notes/A', 'old', now() - interval '1 day');
        INSERT INTO documents (namespace, slug, content) VALUES ('ns', 'notes/a', 'live');
    `); err != nil {
		t.Fatal(err)
	}
	_, err := s.Restore(ctx, "ns", "Notes/A")
	wantStatus(t, err, http.StatusConflict)
	if got := liveSlugs(t, pool, "ns"); !reflect.DeepEqual(got, []string{"notes/a"}) {
		t.Fatalf("live %v", got)
	}
}

// A move that only changes the case of the slug is allowed: the note is not
// a conflict with itself. Its chunks follow.
func TestMoveCanChangeOnlyTheCase(t *testing.T) {
	pool := testutil.ScratchDB(t, slugCaseSchema)
	s := newDeletedCopiesService(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
        INSERT INTO documents (namespace, slug, content) VALUES ('ns', 'notes/a', 'text');
        INSERT INTO memory_chunks (namespace, source_file, chunk_text) VALUES ('ns', 'notes/a', 'text');
    `); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(ctx, MoveInput{Namespace: "ns", Slug: "notes/a", NewSlug: "Notes/A"}); err != nil {
		t.Fatal(err)
	}
	if got := liveSlugs(t, pool, "ns"); !reflect.DeepEqual(got, []string{"Notes/A"}) {
		t.Fatalf("live %v", got)
	}
	if got := chunkFiles(t, pool, "ns"); !reflect.DeepEqual(got, []string{"Notes/A"}) {
		t.Fatalf("chunks %v", got)
	}
	// Another note at the target in any case is still a conflict.
	if _, err := pool.Exec(ctx, "INSERT INTO documents (namespace, slug, content) VALUES ('ns', 'notes/b', 'b')"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Move(ctx, MoveInput{Namespace: "ns", Slug: "Notes/A", NewSlug: "NOTES/B"})
	wantStatus(t, err, http.StatusConflict)
}

// move_folder sees a destination note in another case as a conflict. With
// the exact-case scan it saw none and moved the source onto it, which made
// a second live note (and is now a unique violation).
func TestMovePrefixSeesACaseVariantConflict(t *testing.T) {
	setup := func(t *testing.T) (*Service, *pgxpool.Pool) {
		pool := testutil.ScratchDB(t, slugCaseSchema)
		if _, err := pool.Exec(context.Background(), `
            INSERT INTO documents (namespace, slug, title, content) VALUES ('ns', 'a/x', 'source x', 'x'), ('ns', 'a/y', 'source y', 'y'), ('ns', 'B/x', 'target x', 'bx');
            INSERT INTO memory_chunks (namespace, source_file, chunk_text) VALUES ('ns', 'a/x', 'x'), ('ns', 'a/y', 'y'), ('ns', 'B/x', 'bx');
        `); err != nil {
			t.Fatal(err)
		}
		return newDeletedCopiesService(pool), pool
	}
	ctx := context.Background()

	t.Run("preview lists it", func(t *testing.T) {
		s, _ := setup(t)
		preview, err := s.PreviewMovePrefix(ctx, MovePrefixInput{Namespace: "ns", OldPrefix: "a/", NewPrefix: "b/"})
		if err != nil {
			t.Fatal(err)
		}
		if preview.WouldMove != 2 || len(preview.Conflicts) != 1 || preview.Conflicts[0].Slug != "B/x" {
			t.Fatalf("preview %+v", preview)
		}
	})
	t.Run("skip leaves the source", func(t *testing.T) {
		s, pool := setup(t)
		moved, err := s.MovePrefix(ctx, MovePrefixInput{Namespace: "ns", OldPrefix: "a/", NewPrefix: "b/"})
		if err != nil {
			t.Fatal(err)
		}
		if moved != (PrefixMoved{Moved: 1, Skipped: 1}) {
			t.Fatalf("moved %+v", moved)
		}
		if got := liveSlugs(t, pool, "ns"); !reflect.DeepEqual(got, []string{"B/x", "a/x", "b/y"}) {
			t.Fatalf("live %v", got)
		}
		if got := chunkFiles(t, pool, "ns"); !reflect.DeepEqual(got, []string{"B/x", "a/x", "b/y"}) {
			t.Fatalf("chunks %v", got)
		}
	})
	t.Run("overwrite named in another case", func(t *testing.T) {
		s, pool := setup(t)
		moved, err := s.MovePrefix(ctx, MovePrefixInput{Namespace: "ns", OldPrefix: "a/", NewPrefix: "b/", OverwriteSlugs: []string{"b/X"}})
		if err != nil {
			t.Fatal(err)
		}
		if moved != (PrefixMoved{Moved: 2, Overwritten: 1}) {
			t.Fatalf("moved %+v", moved)
		}
		if got := liveSlugs(t, pool, "ns"); !reflect.DeepEqual(got, []string{"b/x", "b/y"}) {
			t.Fatalf("live %v", got)
		}
		if got := chunkFiles(t, pool, "ns"); !reflect.DeepEqual(got, []string{"b/x", "b/y"}) {
			t.Fatalf("chunks %v", got)
		}
	})
}

// A folder rename that only changes the prefix's case moves every note
// under it; a destination nested under the source in another case is still
// refused.
func TestMovePrefixCanChangeOnlyTheCase(t *testing.T) {
	pool := testutil.ScratchDB(t, slugCaseSchema)
	s := newDeletedCopiesService(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
        INSERT INTO documents (namespace, slug, content) VALUES ('ns', 'notes/x', 'x'), ('ns', 'notes/y', 'y');
        INSERT INTO memory_chunks (namespace, source_file, chunk_text) VALUES ('ns', 'notes/x', 'x');
        INSERT INTO note_synchronization (actor_id, namespace, slug) VALUES (1, 'ns', 'notes/x');
    `); err != nil {
		t.Fatal(err)
	}
	moved, err := s.MovePrefix(ctx, MovePrefixInput{Namespace: "ns", OldPrefix: "notes/", NewPrefix: "Notes/"})
	if err != nil {
		t.Fatal(err)
	}
	if moved != (PrefixMoved{Moved: 2}) {
		t.Fatalf("moved %+v", moved)
	}
	if got := liveSlugs(t, pool, "ns"); !reflect.DeepEqual(got, []string{"Notes/x", "Notes/y"}) {
		t.Fatalf("live %v", got)
	}
	if got := chunkFiles(t, pool, "ns"); !reflect.DeepEqual(got, []string{"Notes/x"}) {
		t.Fatalf("chunks %v", got)
	}
	var synced string
	if err := pool.QueryRow(ctx, "SELECT slug FROM note_synchronization").Scan(&synced); err != nil || synced != "Notes/x" {
		t.Fatalf("sync mapping %q, %v", synced, err)
	}

	_, err = s.PreviewMovePrefix(ctx, MovePrefixInput{Namespace: "ns", OldPrefix: "notes/", NewPrefix: "NOTES/sub/"})
	wantStatus(t, err, http.StatusBadRequest)
}

// The sync mappings of a move_folder: a skipped note keeps its mapping even
// when the mapping spells the slug in another case, and of two mappings of
// one actor that differ only in case (a case-only rename leaves them) only
// the older one moves, so they do not collide at the destination.
func TestMovePrefixSyncMappingsIgnoreCase(t *testing.T) {
	pool := testutil.ScratchDB(t, slugCaseSchema)
	s := newDeletedCopiesService(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
        INSERT INTO documents (namespace, slug, content) VALUES ('ns', 'a/x', 'x'), ('ns', 'a/y', 'y'), ('ns', 'B/x', 'bx');
        INSERT INTO note_synchronization (actor_id, namespace, slug) VALUES (1, 'ns', 'A/x'), (1, 'ns', 'a/y'), (1, 'ns', 'A/Y');
    `); err != nil {
		t.Fatal(err)
	}
	moved, err := s.MovePrefix(ctx, MovePrefixInput{Namespace: "ns", OldPrefix: "a/", NewPrefix: "b/"})
	if err != nil {
		t.Fatal(err)
	}
	if moved != (PrefixMoved{Moved: 1, Skipped: 1}) {
		t.Fatalf("moved %+v", moved)
	}
	if got := liveSlugs(t, pool, "ns"); !reflect.DeepEqual(got, []string{"B/x", "a/x", "b/y"}) {
		t.Fatalf("live %v", got)
	}
	if got := syncSlugs(t, pool, 1, "ns"); !reflect.DeepEqual(got, []string{"A/Y", "A/x", "b/y"}) {
		t.Fatalf("sync mappings %v", got)
	}
}

// Under a Turkish collation Postgres lowers "I" to dotless "ı", where
// Go's strings.ToLower gives "i". move_folder's nesting guard and its
// overwrite list must follow Postgres, which picks the notes; with Go's rule
// the guard refused this move and the overwrite missed its destination.
func TestMovePrefixFollowsTheDatabaseCollation(t *testing.T) {
	pool := testutil.ScratchDBICU(t, "tr-TR", slugCaseSchema)
	s := newDeletedCopiesService(pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
        INSERT INTO documents (namespace, slug, content) VALUES ('ns', 'i/x', 'x'), ('ns', 'a/I', 'source'), ('ns', 'b/ı', 'target');
    `); err != nil {
		t.Fatal(err)
	}

	// "I/sub/" is not under "i/" in Turkish: LOWER gives "ı/sub/".
	moved, err := s.MovePrefix(ctx, MovePrefixInput{Namespace: "ns", OldPrefix: "i/", NewPrefix: "I/sub/"})
	if err != nil {
		t.Fatal(err)
	}
	if moved != (PrefixMoved{Moved: 1}) {
		t.Fatalf("nested move %+v", moved)
	}

	// "b/I" names the live "b/ı" in Turkish.
	moved, err = s.MovePrefix(ctx, MovePrefixInput{Namespace: "ns", OldPrefix: "a/", NewPrefix: "b/", OverwriteSlugs: []string{"b/I"}})
	if err != nil {
		t.Fatal(err)
	}
	if moved != (PrefixMoved{Moved: 1, Overwritten: 1}) {
		t.Fatalf("overwrite move %+v", moved)
	}
	if got := liveSlugs(t, pool, "ns"); !reflect.DeepEqual(got, []string{"I/sub/x", "b/I"}) {
		t.Fatalf("live %v", got)
	}
}
