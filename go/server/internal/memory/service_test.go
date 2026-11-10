package memory

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// fixedEmbedder answers every text with the same short vector.
type fixedEmbedder struct{}

func (fixedEmbedder) Embed(ctx context.Context, texts []string) ([][]float64, error) {
	out := make([][]float64, len(texts))
	for i := range texts {
		out[i] = []float64{0.1, 0.2, 0.3}
	}
	return out, nil
}

const (
	ingestLockSQL   = `SELECT pg_advisory_xact_lock($1, hashtext($2::text || '/' || LOWER($3::text)))`
	ingestDeleteSQL = "DELETE FROM memory_chunks WHERE namespace = $1 AND LOWER(source_file) = LOWER($2)"
	ingestInsertSQL = "INSERT INTO memory_chunks (namespace, source_file, heading, chunk_text, embedding) VALUES ($1, $2, $3, $4, $5)"
)

// Ingest takes the per-file lock inside its transaction, before it deletes
// the old chunks (LLM-919). pgxmock checks the order. The lock itself, on a
// real database, is in service_db_test.go.
func TestIngestLocksBeforeDelete(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(ingestLockSQL)).WithArgs(919001, "agent", "Notes/Race.md").
		WillReturnResult(pgxmock.NewResult("SELECT", 1))
	mock.ExpectExec(regexp.QuoteMeta(ingestDeleteSQL)).WithArgs("agent", "Notes/Race.md").
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	for range 2 {
		mock.ExpectExec(regexp.QuoteMeta(ingestInsertSQL)).WithArgs("agent", "Notes/Race.md", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
	}
	mock.ExpectCommit()

	s := &Service{DB: mock, Embedder: fixedEmbedder{}}
	res, err := s.Ingest(context.Background(), "agent", "Notes/Race.md", "# One\n\nfirst\n\n# Two\n\nsecond")
	if err != nil {
		t.Fatal(err)
	}
	if res.ChunksCreated != 2 {
		t.Errorf("chunks_created = %d, want 2", res.ChunksCreated)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// A failed lock rolls back without deleting anything.
func TestIngestLockFails(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(ingestLockSQL)).WithArgs(919001, "agent", "race.md").
		WillReturnError(errors.New("simulated lock failure"))
	mock.ExpectRollback()

	s := &Service{DB: mock, Embedder: fixedEmbedder{}}
	if _, err := s.Ingest(context.Background(), "agent", "race.md", "# One\n\nfirst"); err == nil || err.Error() != "simulated lock failure" {
		t.Fatalf("err = %v, want the lock failure", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
