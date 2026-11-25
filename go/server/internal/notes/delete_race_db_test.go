package notes

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// A note is deleted and a new note is saved at the same slug. The delete's
// chunk removal is paused, and the new note's save and re-index run in that
// pause. The new note must keep its chunks. When the removal was a
// background job keyed by slug, it ran after the new note's re-index and
// took its chunks; the note stayed live with nothing to search.
func TestDeleteLeavesChunksOfANoteSavedAtTheSlugAfterIt(t *testing.T) {
	pool := testutil.ScratchDB(t, deletedCopiesSchema)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "INSERT INTO documents (namespace, slug, title, content) VALUES ('ns', 'notes/a', 'Old', 'old text')"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO memory_chunks (namespace, source_file, chunk_text) VALUES ('ns', 'notes/a', 'old text')"); err != nil {
		t.Fatal(err)
	}

	// Only the notes service's DB pauses; the re-index runs on the memory
	// service's DB, whose own DELETE FROM memory_chunks must not pause.
	s := newDeletedCopiesService(pool)
	paused := newPausingDB(pool, "DELETE FROM memory_chunks")
	s.DB = paused
	// Deferred before the delete starts: a failed assertion below must not
	// leave a paused transaction open while the scratch database is dropped.
	defer paused.Release()

	deleteDone := make(chan struct{})
	var deleteErr error
	go func() {
		defer close(deleteDone)
		_, deleteErr = s.Delete(ctx, "ns", "notes/a", nil)
	}()
	waitClosed(t, paused.reached, "the delete reaching its chunk removal")

	// The new note as Save leaves it: a live row at the slug, then the
	// re-index Save starts for it in the background. (Save itself needs
	// quota tables this schema does not have.) The insert may wait on the
	// deleted row until the delete commits.
	saveDone := make(chan struct{})
	var saveErr error
	var reindexed int
	go func() {
		defer close(saveDone)
		var newID int
		if saveErr = pool.QueryRow(ctx, "INSERT INTO documents (namespace, slug, title, content) VALUES ('ns', 'notes/a', 'New', 'new text') RETURNING id").Scan(&newID); saveErr != nil {
			return
		}
		res, err := s.Memory.IngestDocument(ctx, newID, "ns", "notes/a", "new text")
		saveErr, reindexed = err, res.ChunksCreated
	}()
	waitDoneOrBlocked(t, pool, saveDone)
	paused.Release()
	waitClosed(t, deleteDone, "the delete finishing")
	waitClosed(t, saveDone, "the new note's save and re-index finishing")
	waitClosed(t, paused.finished, "the delete's chunk removal finishing")
	if deleteErr != nil {
		t.Fatalf("delete: %v", deleteErr)
	}
	if saveErr != nil || reindexed != 1 {
		t.Fatalf("save and re-index of the new note: %d chunks, %v", reindexed, saveErr)
	}

	if texts := chunkTexts(t, pool); len(texts) != 1 || texts[0] != "new text" {
		t.Errorf("the live note at notes/a has chunks %q, want [\"new text\"]", texts)
	}
}

func chunkTexts(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), "SELECT chunk_text FROM memory_chunks ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	texts, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return texts
}
