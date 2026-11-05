package syserrors

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

func TestRecordWithoutAgent(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectExec("INSERT INTO system_errors").
		WithArgs((*int)(nil), "documents", "AUTO_INDEX_FAILED", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	rec := &Recorder{DB: mock}
	rec.Record(context.Background(), "", "documents", "AUTO_INDEX_FAILED", map[string]string{"error": "boom"})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordNilContextIsNull(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectExec("INSERT INTO system_errors").
		WithArgs((*int)(nil), "memory", "X", (*string)(nil)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	var logged string
	rec := &Recorder{DB: mock, Errorf: func(f string, a ...any) { logged = f }}
	rec.Record(context.Background(), "", "memory", "X", nil)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if logged != "" {
		t.Errorf("logged on success: %s", logged)
	}
}
