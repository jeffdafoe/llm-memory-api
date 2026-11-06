package visibility

import (
	"context"
	"reflect"
	"regexp"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

type super bool

func (s super) IsSuperadmin(context.Context, int) (bool, error) { return bool(s), nil }

func expect(mock pgxmock.PgxPoolIface, viewer int, peers, created []int, targets []*int) {
	peerRows := pgxmock.NewRows([]string{"id"})
	for _, id := range peers {
		peerRows.AddRow(id)
	}
	mock.ExpectQuery(regexp.QuoteMeta("JOIN actors a2 ON a1.realms && a2.realms")).WithArgs(viewer).WillReturnRows(peerRows)
	createdRows := pgxmock.NewRows([]string{"id"})
	for _, id := range created {
		createdRows.AddRow(id)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM actors WHERE created_by = $1")).WithArgs(viewer).WillReturnRows(createdRows)
	targetRows := pgxmock.NewRows([]string{"target_actor_id"})
	for _, id := range targets {
		targetRows.AddRow(id)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT target_actor_id FROM actor_visibility_configuration WHERE actor_id = $1")).WithArgs(viewer).WillReturnRows(targetRows)
}

func ptr(n int) *int { return &n }

func TestVisibleActorIDs(t *testing.T) {
	cases := []struct {
		name    string
		peers   []int
		created []int
		targets []*int
		want    []int
	}{
		// No grant rows: self and created actors only, even across realms.
		{"default deny", []int{1, 5, 6}, []int{9}, nil, []int{1, 9}},
		// A wildcard: every realm peer, then the created actors.
		{"wildcard", []int{5, 1, 6}, []int{9, 5}, []*int{ptr(6), nil}, []int{1, 5, 6, 9}},
		// Explicit grants count only inside the realm.
		{"grants", []int{1, 5, 6}, nil, []*int{ptr(6), ptr(7)}, []int{1, 6}},
	}
	for _, c := range cases {
		mock := testutil.NewMock(t)
		v := &Visibility{DB: mock, Admin: super(false)}
		expect(mock, 1, c.peers, c.created, c.targets)
		got, err := v.VisibleActorIDs(context.Background(), 1)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.All || !reflect.DeepEqual(got.IDs, c.want) {
			t.Errorf("%s: %+v", c.name, got)
		}
		// The second call is served from the cache: no more queries.
		if _, err := v.VisibleActorIDs(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

func TestSuperadminSeesAll(t *testing.T) {
	mock := testutil.NewMock(t)
	v := &Visibility{DB: mock, Admin: super(true)}
	got, err := v.VisibleActorIDs(context.Background(), 1)
	if err != nil || !got.All || !got.Has(12345) {
		t.Errorf("%+v %v", got, err)
	}
}

func TestInvalidViewerSeesNothing(t *testing.T) {
	v := &Visibility{Admin: super(true)}
	got, err := v.VisibleActorIDs(context.Background(), 0)
	if err != nil || got.All || len(got.IDs) != 0 {
		t.Errorf("%+v %v", got, err)
	}
	if ok, _ := v.CanSee(context.Background(), 0, 1); ok {
		t.Error("invalid viewer sees")
	}
}

func TestCanSeeAndClearCache(t *testing.T) {
	mock := testutil.NewMock(t)
	v := &Visibility{DB: mock, Admin: super(false)}
	// Every actor sees itself without a query.
	if ok, err := v.CanSee(context.Background(), 1, 1); !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	expect(mock, 1, []int{1, 2}, nil, []*int{ptr(2)})
	if ok, _ := v.CanSee(context.Background(), 1, 2); !ok {
		t.Error("granted peer not visible")
	}
	if ok, _ := v.CanSee(context.Background(), 1, 3); ok {
		t.Error("other actor visible")
	}
	v.ClearCache(1)
	expect(mock, 1, []int{1, 2}, nil, nil)
	if ok, _ := v.CanSee(context.Background(), 1, 2); ok {
		t.Error("revoked grant still visible after ClearCache")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}
