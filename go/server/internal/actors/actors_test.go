package actors

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

func TestResolveByNameCachesHits(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("work").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(4, "work"))

	cache := NewCache(mock)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		actor, ok, err := cache.ResolveByName(ctx, "work")
		if err != nil || !ok || actor.ID != 4 {
			t.Fatalf("lookup %d = %+v, %v, %v", i, actor, ok, err)
		}
	}
	// The ID side of the cache was filled by the name lookup.
	actor, ok, err := cache.ResolveByID(ctx, 4)
	if err != nil || !ok || actor.Name != "work" {
		t.Fatalf("ResolveByID = %+v, %v, %v", actor, ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestResolveByNameExpires(t *testing.T) {
	mock := testutil.NewMock(t)
	rows := func() *pgxmock.Rows {
		return pgxmock.NewRows([]string{"id", "name"}).AddRow(4, "work")
	}
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("work").WillReturnRows(rows())
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("work").WillReturnRows(rows())

	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	cache := NewCache(mock)
	cache.entries.Now = func() time.Time { return now }
	ctx := context.Background()
	cache.ResolveByName(ctx, "work")
	now = now.Add(cacheTTL + time.Second)
	cache.ResolveByName(ctx, "work")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMissIsNotCached(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("ghost").WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("ghost").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(9, "ghost"))

	cache := NewCache(mock)
	ctx := context.Background()
	_, ok, err := cache.ResolveByName(ctx, "ghost")
	if err != nil || ok {
		t.Fatalf("first lookup = %v, %v; want not found", ok, err)
	}
	actor, ok, err := cache.ResolveByName(ctx, "ghost")
	if err != nil || !ok || actor.ID != 9 {
		t.Fatalf("second lookup = %+v, %v, %v", actor, ok, err)
	}
}

func TestRequireByNameNotFound(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("ghost").WillReturnError(pgx.ErrNoRows)

	_, err := NewCache(mock).RequireByName(context.Background(), "ghost")
	status, code := apierr.StatusAndCode(err)
	if status != http.StatusNotFound || code != "NOT_FOUND" {
		t.Fatalf("status, code = %d, %s", status, code)
	}
	if err.Error() != `Agent "ghost" is not registered` {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestResolveMultipleByNameQueriesOnlyUncached(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name =").WithArgs("home").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(3, "home"))
	mock.ExpectQuery("name = ANY").WithArgs([]string{"work", "ghost"}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(4, "work"))

	cache := NewCache(mock)
	ctx := context.Background()
	cache.ResolveByName(ctx, "home")
	got, err := cache.ResolveMultipleByName(ctx, []string{"home", "work", "ghost"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["home"].ID != 3 || got["work"].ID != 4 {
		t.Fatalf("got %+v", got)
	}
	if _, ok := got["ghost"]; ok {
		t.Fatal("ghost should be absent")
	}
}
