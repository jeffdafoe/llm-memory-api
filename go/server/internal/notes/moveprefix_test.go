package notes

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// expectPrefixScan answers the prefix scan: the nesting guard (not nested),
// the count and the conflicts. Each conflict is a destination slug; the
// source that would move onto it is the same rest under oldPrefix, and it
// is an overwrite when overwrites names it in any case (Postgres decides
// that in the real query).
func expectPrefixScan(mock pgxmock.PgxPoolIface, ns, oldPrefix, newPrefix, oldLike string, sources int64, overwrites []string, conflicts ...string) {
	if newPrefix != "" {
		mock.ExpectQuery(`SELECT LOWER\(\$1\) = LOWER\(\$2\)`).WithArgs(oldPrefix, newPrefix).
			WillReturnRows(pgxmock.NewRows([]string{"same", "nested"}).AddRow(false, false))
	}
	mock.ExpectExec(`SELECT slug, title FROM documents`).WithArgs(ns, oldLike).
		WillReturnResult(pgxmock.NewResult("SELECT", sources))
	rows := pgxmock.NewRows([]string{"slug", "title", "source", "overwrite"})
	for _, c := range conflicts {
		title := "T " + c
		overwrite := false
		for _, o := range overwrites {
			overwrite = overwrite || strings.EqualFold(o, c)
		}
		rows.AddRow(c, &title, oldPrefix+strings.TrimPrefix(c, newPrefix), overwrite)
	}
	if overwrites == nil {
		overwrites = []string{}
	}
	mock.ExpectQuery(`SELECT d2.slug, d2.title, d1.slug,`).WithArgs(ns, oldPrefix, newPrefix, oldLike, overwrites).WillReturnRows(rows)
}

func TestMovePrefixMovesEverythingInOneTransaction(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.MatchExpectationsInOrder(true)
	expectPrefixScan(mock, "ns", "a_b/", "c/", `a\_b/`, 2, nil)
	mock.ExpectBegin()
	mock.ExpectQuery(`UPDATE documents d\s+SET slug = \$3`).WithArgs("ns", "a_b/", "c/", `a\_b/`, []string{}).
		WillReturnRows(pgxmock.NewRows([]string{"old", "new"}).AddRow("a_b/x", "c/x").AddRow("A_B/Y", "c/Y"))
	mock.ExpectExec(`SET source_file = m.new_slug`).WithArgs("ns", []string{"a_b/x", "A_B/Y"}, []string{"c/x", "c/Y"}).WillReturnResult(pgxmock.NewResult("UPDATE", 5))
	mock.ExpectExec(`SET source_file = \$3 \|\|`).WithArgs("ns", "a_b/", "c/", `a\_b/`).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(`UPDATE note_synchronization src`).WithArgs("ns", []string{"a_b/x", "A_B/Y"}, []string{"c/x", "c/Y"}).WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectCommit()

	s := &Service{DB: mock}
	got, err := s.MovePrefix(context.Background(), MovePrefixInput{Namespace: "ns", OldPrefix: "a_b/", NewPrefix: "c/"})
	if err != nil {
		t.Fatal(err)
	}
	if got != (PrefixMoved{Moved: 2}) {
		t.Fatalf("got %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMovePrefixOverwritesListedConflictsAndSkipsTheRest(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.MatchExpectationsInOrder(true)
	expectPrefixScan(mock, "ns", "a/", "b/", "a/", 3, []string{"B/Over"}, "b/keep", "b/over")
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE documents SET deleted_at = NOW\(\)`).WithArgs("ns", []string{"b/over"}).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`DELETE FROM memory_chunks`).WithArgs("ns", []string{"b/over"}).WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectQuery(`d.slug != ALL\(\$5\)`).WithArgs("ns", "a/", "b/", "a/", []string{"a/keep"}).
		WillReturnRows(pgxmock.NewRows([]string{"old", "new"}).AddRow("a/over", "b/over").AddRow("a/z", "b/z"))
	mock.ExpectExec(`SET source_file = m.new_slug`).WithArgs("ns", []string{"a/over", "a/z"}, []string{"b/over", "b/z"}).WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	mock.ExpectExec(`SET source_file = \$3 \|\|`).WithArgs("ns", "a/", "b/", "a/").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(`UPDATE note_synchronization src`).WithArgs("ns", []string{"a/over", "a/z"}, []string{"b/over", "b/z"}).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	s := &Service{DB: mock}
	// Named in another case: slugs match without case.
	got, err := s.MovePrefix(context.Background(), MovePrefixInput{Namespace: "ns", OldPrefix: "a/", NewPrefix: "b/", OverwriteSlugs: []string{"B/Over"}})
	if err != nil {
		t.Fatal(err)
	}
	if got != (PrefixMoved{Moved: 2, Skipped: 1, Overwritten: 1}) {
		t.Fatalf("got %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMovePrefixFailedWriteRollsBack(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.MatchExpectationsInOrder(true)
	expectPrefixScan(mock, "ns", "a/", "", "a/", 1, nil)
	mock.ExpectBegin()
	mock.ExpectQuery(`UPDATE documents d\s+SET slug = \$3`).WithArgs("ns", "a/", "", "a/", []string{}).
		WillReturnRows(pgxmock.NewRows([]string{"old", "new"}).AddRow("a/x", "x"))
	mock.ExpectExec(`SET source_file = m.new_slug`).WithArgs("ns", []string{"a/x"}, []string{"x"}).WillReturnError(errors.New("connection reset"))
	mock.ExpectRollback()

	s := &Service{DB: mock}
	if _, err := s.MovePrefix(context.Background(), MovePrefixInput{Namespace: "ns", OldPrefix: "a/", NewPrefix: ""}); err == nil || err.Error() != "connection reset" {
		t.Fatalf("err %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewMovePrefixRefusesANestedDestination(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery(`SELECT LOWER\(\$1\) = LOWER\(\$2\)`).WithArgs("a/", "a/sub/").
		WillReturnRows(pgxmock.NewRows([]string{"same", "nested"}).AddRow(false, true))
	s := &Service{DB: mock}
	_, err := s.PreviewMovePrefix(context.Background(), MovePrefixInput{Namespace: "ns", OldPrefix: "a/", NewPrefix: "a/sub/"})
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Status != 400 ||
		apiErr.Message != `Cannot move prefix "a/" to "a/sub/": the destination is the same as, or nested under, the source.` {
		t.Fatalf("err %v", err)
	}
	if _, err := s.PreviewMovePrefix(context.Background(), MovePrefixInput{Namespace: "ns", OldPrefix: "/a", NewPrefix: "b/"}); err == nil {
		t.Fatal("a leading slash passed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
