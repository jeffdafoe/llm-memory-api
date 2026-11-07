package admin

import (
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

func TestValidAdminPerm(t *testing.T) {
	cases := []struct {
		resource, action string
		ok               bool
		err              string
	}{
		{"*", "*", true, ""}, {"agents", "write", true, ""}, {"plugins", "administer", true, ""},
		{"agents", "delete", false, ""}, {"bogus", "read", false, ""}, {"*", "read", false, ""},
		{"constructor", "read", false, "allowed.includes is not a function"},
		{"__proto__", "x", false, "allowed.includes is not a function"},
	}
	for _, c := range cases {
		ok, err := validAdminPerm(c.resource, c.action)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if ok != c.ok || got != c.err {
			t.Errorf("%s:%s = %v %q", c.resource, c.action, ok, got)
		}
	}
}

func TestStrictlyEquals(t *testing.T) {
	for raw, want := range map[string]bool{`2`: true, `2.0`: true, `"2"`: false, `[2]`: false, `3`: false, ``: false} {
		if strictlyEquals(json.RawMessage(raw), 2) != want {
			t.Errorf("%s === 2 should be %v", raw, want)
		}
	}
}

// The superadmin-only saves refuse before reading the body.
func TestSuperadminOnly(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	grants(mock, 2, "agents", "write")
	rec := httptest.NewRecorder()
	if err := rt.actorsPermissionsSave(rec, request("")); err != nil {
		t.Fatal(err)
	}
	check(t, "namespace perms", rec, 403, errorBody("FORBIDDEN", "Managing namespace permissions requires superadmin"))
	rec = httptest.NewRecorder()
	if err := rt.actorsAdminPermissionsSave(rec, request("")); err != nil {
		t.Fatal(err)
	}
	check(t, "admin perms", rec, 403, errorBody("FORBIDDEN", "Managing admin permissions requires superadmin"))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func actorRow(mock pgxmock.PgxPoolIface, id int, name string) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, name FROM actors WHERE id = $1")).WithArgs(id).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(id, name))
}

func TestPermissionsSaveValidation(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	grants(mock, 2, "*", "*")
	cases := []struct {
		name, body string
		status     int
		want       string
	}{
		{"wildcard search", `{"actor_id":5,"permissions":[{"namespace":"*"}]}`, 400, errorBody("BAD_REQUEST", `"*" is a reserved namespace and cannot be used for content`)},
		{"blank", `{"actor_id":5,"permissions":[{"namespace":"  "}]}`, 400, errorBody("BAD_REQUEST", "Empty namespace not allowed")},
		{"not an object", `{"actor_id":5,"permissions":["shared"]}`, 400, errorBody("BAD_REQUEST", "Empty namespace not allowed")},
		{"duplicate after trim", `{"actor_id":5,"permissions":[{"namespace":"x"},{"namespace":" x "},{"namespace":"/"}]}`, 400, errorBody("BAD_REQUEST", "Duplicate namespace entries")},
	}
	// The actor cache answers the lookups after the first.
	actorRow(mock, 5, "agent-x")
	for _, c := range cases {
		rec := httptest.NewRecorder()
		if err := rt.actorsPermissionsSave(rec, request(c.body)); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		check(t, c.name, rec, c.status, c.want)
	}
	err := rt.actorsPermissionsSave(httptest.NewRecorder(), request(`{"actor_id":5,"permissions":[{"namespace":"x"},null]}`))
	if err == nil || err.Error() != "Cannot read properties of null (reading 'namespace')" {
		t.Fatalf("null element: %v", err)
	}
	rec := httptest.NewRecorder()
	rt.actorsPermissionsSave(rec, request(`{"actor_id":"x","permissions":[]}`))
	check(t, "bad id", rec, 400, errorBody("BAD_REQUEST", "Invalid actor_id"))
	rec = httptest.NewRecorder()
	rt.actorsPermissionsSave(rec, request(`{"actor_id":5,"permissions":{}}`))
	check(t, "not array", rec, 400, errorBody("BAD_REQUEST", "Required field: permissions (array)"))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPermissionsSaveWrites(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	grants(mock, 2, "*", "*")
	actorRow(mock, 5, "agent-x")
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM namespace_permissions WHERE actor_id = $1")).WithArgs(5).
		WillReturnResult(pgxmock.NewResult("DELETE", 2))
	insert := regexp.QuoteMeta("INSERT INTO namespace_permissions (actor_id, namespace, can_read, can_write, can_delete) VALUES ($1, $2, $3, $4, $5)")
	mock.ExpectExec(insert).WithArgs(5, "shared", true, false, false).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(insert).WithArgs(5, "/", false, true, true).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()
	rec := httptest.NewRecorder()
	if err := rt.actorsPermissionsSave(rec, request(`{"actor_id":"5","permissions":[{"namespace":" shared ","can_read":1},{"namespace":"/","can_write":"y","can_delete":[]}]}`)); err != nil {
		t.Fatal(err)
	}
	check(t, "save", rec, 200, `{"ok":true}`)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestVisibilitySaveValidation(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	for _, body := range []string{`{"actor_id":5,"wildcard":"true","grants":[]}`, `{"actor_id":5,"wildcard":false}`, `{"actor_id":5,"wildcard":1,"grants":[]}`} {
		rec := httptest.NewRecorder()
		rt.actorsVisibilitySave(rec, request(body))
		check(t, body, rec, 400, errorBody("BAD_REQUEST", "Required fields: wildcard (bool), grants (array of actor IDs)"))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteRefusals(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	rec := httptest.NewRecorder()
	rt.actorsDelete(rec, request(`{"actor_id":2}`))
	check(t, "self", rec, 400, errorBody("BAD_REQUEST", "Cannot delete your own account"))
	rec = httptest.NewRecorder()
	rt.actorsDelete(rec, request(`{"actor_id":"1.5"}`))
	check(t, "bad id", rec, 400, errorBody("BAD_REQUEST", "Required field: actor_id (positive integer)"))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A deleted actor's cached tokens are dropped along with its owned
// virtual agents'.
func TestDeleteForgetsTokens(t *testing.T) {
	rt, mock, tokens := newRoutes(t, nil)
	actorRow(mock, 5, "agent-x")
	grants(mock, 2, "*", "*")
	mock.ExpectQuery("WHERE a.created_by = \\$1 AND ac.virtual = true").WithArgs(5).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(6, "x-va"))
	mock.ExpectBegin()
	for _, id := range []int{5, 6} {
		for _, sql := range actorDeleteSteps {
			mock.ExpectExec(regexp.QuoteMeta(sql)).WithArgs(id).WillReturnResult(pgxmock.NewResult("DELETE", 0))
		}
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM discussions WHERE created_by_actor_id = $1")).WithArgs(id).WillReturnResult(pgxmock.NewResult("DELETE", 0))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM agent_configuration WHERE actor_id = $1")).WithArgs(id).WillReturnResult(pgxmock.NewResult("DELETE", 1))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM actors WHERE id = $1")).WithArgs(id).WillReturnResult(pgxmock.NewResult("DELETE", 1))
	}
	for _, n := range []string{"agent-x", "x-va"} {
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM memory_chunks WHERE namespace = $1")).WithArgs(n).WillReturnResult(pgxmock.NewResult("DELETE", 0))
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM documents WHERE namespace = $1")).WithArgs(n).WillReturnResult(pgxmock.NewResult("DELETE", 0))
	}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE actors SET created_by = NULL WHERE created_by = ANY($1::int[])")).WithArgs([]int{5, 6}).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectCommit()
	rec := httptest.NewRecorder()
	if err := rt.actorsDelete(rec, request(`{"actor_id":5}`)); err != nil {
		t.Fatal(err)
	}
	check(t, "delete", rec, 200, `{"message":"Actor deleted","deleted":{"actor":"agent-x","virtual_agents":["x-va"]}}`)
	if len(tokens.actors) != 2 || tokens.actors[0] != 5 || tokens.actors[1] != 6 {
		t.Fatalf("forgot %v", tokens.actors)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestVAAccessPlainErrors(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	rec := httptest.NewRecorder()
	rt.vaAccessGrant(rec, request(`{}`))
	check(t, "grant missing", rec, 400, `{"error":"virtual_agent_id required"}`)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT virtual FROM agent_configuration WHERE actor_id = $1")).WithArgs(boundNumber("3")).
		WillReturnRows(pgxmock.NewRows([]string{"virtual"}).AddRow(false))
	rec = httptest.NewRecorder()
	rt.vaAccessGrant(rec, request(`{"virtual_agent_id":3}`))
	check(t, "not virtual", rec, 400, `{"error":"Not a virtual agent"}`)
	rec = httptest.NewRecorder()
	rt.vaAccessRevoke(rec, request(`{"id":0}`))
	check(t, "revoke missing", rec, 400, `{"error":"id required"}`)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT virtual_agent_id FROM virtual_agent_access WHERE id = $1")).WithArgs(boundNumber("9")).
		WillReturnRows(pgxmock.NewRows([]string{"virtual_agent_id"}))
	rec = httptest.NewRecorder()
	rt.vaAccessRevoke(rec, request(`{"id":9}`))
	check(t, "revoke unknown", rec, 200, `{"revoked":true}`)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func boundNumber(s string) jsutil.Number { return jsutil.Number(s) }
