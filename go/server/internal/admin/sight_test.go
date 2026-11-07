package admin

import (
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/pashagolub/pgxmock/v6"
)

// limitedSight is the visibility load for user 2 as a limited admin: no *:*
// grant, no realm peers, it created actor 5, no visibility rows. It sees 2
// and 5.
func limitedSight(mock pgxmock.PgxPoolIface) {
	grants(mock, 2, "agents", "read")
	mock.ExpectQuery(regexp.QuoteMeta("SELECT a2.id FROM actors a1")).WithArgs(2).
		WillReturnRows(pgxmock.NewRows([]string{"id"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM actors WHERE created_by = $1")).WithArgs(2).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(5))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT target_actor_id FROM actor_visibility_configuration WHERE actor_id = $1")).WithArgs(2).
		WillReturnRows(pgxmock.NewRows([]string{"target_actor_id"}))
}

// A limited admin asking about an actor it cannot see gets the 404 of a
// missing actor, and the route reads none of that actor's rows.
func TestActorReadsNeedSight(t *testing.T) {
	notFound := errorBody("NOT_FOUND", "Actor not found")
	resolve := func(mock pgxmock.PgxPoolIface, id int, name string) {
		mock.ExpectQuery(regexp.QuoteMeta("SELECT id, name FROM actors WHERE id = $1")).WithArgs(id).
			WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(id, name))
	}
	cases := []struct {
		name   string
		fn     func(*Routes, *httptest.ResponseRecorder, string) error
		lookup bool
	}{
		{"permissions read", func(rt *Routes, w *httptest.ResponseRecorder, b string) error {
			return rt.actorsPermissionsRead(w, request(b))
		}, true},
		{"visibility read", func(rt *Routes, w *httptest.ResponseRecorder, b string) error {
			return rt.actorsVisibilityRead(w, request(b))
		}, true},
		{"admin permissions read", func(rt *Routes, w *httptest.ResponseRecorder, b string) error {
			return rt.actorsAdminPermissionsRead(w, request(b))
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt, mock, _ := newRoutes(t, nil)
			if c.lookup {
				resolve(mock, 9, "agent-a")
			}
			limitedSight(mock)
			rec := httptest.NewRecorder()
			if err := c.fn(rt, rec, `{"actor_id":9}`); err != nil {
				t.Fatal(err)
			}
			check(t, c.name, rec, 404, notFound)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Call detail and the access list are filtered to the agents the caller
// can see; a superadmin's are not.
func TestCallDetailAndAccessListNeedSight(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	limitedSight(mock)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE c.id = $1 AND c.actor_id = ANY($2)")).WithArgs(int64(9101), []int{2, 5}).
		WillReturnRows(pgxmock.NewRows([]string{"id"}))
	rec := httptest.NewRecorder()
	if err := rt.agentsCallDetail(rec, request(`{"call_id":9101}`)); err != nil {
		t.Fatal(err)
	}
	check(t, "hidden call", rec, 404, errorBody("NOT_FOUND", "Call detail not found (may have been purged)"))

	mock.ExpectQuery(regexp.QuoteMeta("WHERE vaa.virtual_agent_id = ANY($1) ORDER BY va.name, ga.name NULLS FIRST")).WithArgs([]int{2, 5}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "virtual_agent_id", "virtual_agent_name", "grantee_actor_id", "grantee_name", "created_at"}))
	rec = httptest.NewRecorder()
	if err := rt.vaAccessList(rec, request(`{}`)); err != nil {
		t.Fatal(err)
	}
	check(t, "access list", rec, 200, `{"access":[]}`)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	super, mock, _ := newRoutes(t, nil)
	grants(mock, 2, "*", "*")
	mock.ExpectQuery(`(?s)WHERE c\.id = \$1$`).WithArgs(int64(9101)).
		WillReturnRows(pgxmock.NewRows([]string{"id"}))
	rec = httptest.NewRecorder()
	if err := super.agentsCallDetail(rec, request(`{"call_id":9101}`)); err != nil {
		t.Fatal(err)
	}
	check(t, "superadmin call", rec, 404, errorBody("NOT_FOUND", "Call detail not found (may have been purged)"))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
