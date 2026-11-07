package admin

import (
	"context"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
)

// serveHungUp sends body to the registered route at path (below /v1) and
// cancels the request's context 10 ms in, while the check the case delays
// is in flight. The route goes through the admin mux, so what Register puts
// in front of the handler (the permission check included) is covered.
func serveHungUp(t *testing.T, rt *Routes, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := NewMux()
	rt.Register(mux)
	r := httptest.NewRequest("POST", "/v1"+path, nil)
	st := &httpapi.State{ActorID: 2, AuthenticatedUser: &httpapi.User{ID: 2, Username: "admin"}}
	st.Body = []byte(body)
	r = httpapi.WithStateValue(r, st)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r.WithContext(ctx))
	return rec
}

// delayed is a query the client hangs up during.
const delayed = 50 * time.Millisecond

// A client that hangs up while a route checks permission, visibility,
// ownership or existence does not stop the write after the check: Node's
// pool queries ran on after a disconnect, so the write landed.
func TestWritesOutliveAHangUpAtTheCheck(t *testing.T) {
	nameLookup := regexp.QuoteMeta("SELECT id, name FROM actors WHERE name = $1")
	configuration := regexp.QuoteMeta("SELECT actor_id, virtual FROM agent_configuration WHERE actor_id = $1")
	cases := []struct {
		name, path, body string
		config           map[string]string
		expect           func(mock pgxmock.PgxPoolIface)
		status           int
	}{
		{
			name: "permission check", path: "/admin/config/update", body: `{"key":"a","value":"new"}`,
			config: map[string]string{"a": "old"},
			expect: func(mock pgxmock.PgxPoolIface) {
				mock.ExpectQuery(regexp.QuoteMeta("SELECT resource, action FROM admin_permissions WHERE actor_id = $1")).WithArgs(2).
					WillDelayFor(delayed).WillReturnRows(pgxmock.NewRows([]string{"resource", "action"}).AddRow("config", "write"))
				mock.ExpectExec(regexp.QuoteMeta("UPDATE config SET value = $1 WHERE key = $2")).WithArgs("new", "a").
					WillReturnResult(pgxmock.NewResult("UPDATE", 1))
			},
			status: 200,
		},
		{
			name: "agents/update visibility check", path: "/admin/agents/update", body: `{"agent":"admin","model":"m"}`,
			expect: func(mock pgxmock.PgxPoolIface) {
				grants(mock, 2, "agents", "write")
				mock.ExpectQuery(nameLookup).WithArgs("admin").WillDelayFor(delayed).
					WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(2, "admin"))
				mock.ExpectQuery(configuration).WithArgs(2).
					WillReturnRows(pgxmock.NewRows([]string{"actor_id", "virtual"}).AddRow(2, false))
				mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_configuration SET model = $1 WHERE actor_id = $2")).WithArgs("m", 2).
					WillReturnResult(pgxmock.NewResult("UPDATE", 1))
			},
			status: 200,
		},
		{
			name: "agents/update existence check", path: "/admin/agents/update", body: `{"agent":"admin","model":"m"}`,
			expect: func(mock pgxmock.PgxPoolIface) {
				grants(mock, 2, "agents", "write")
				mock.ExpectQuery(nameLookup).WithArgs("admin").
					WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(2, "admin"))
				mock.ExpectQuery(configuration).WithArgs(2).WillDelayFor(delayed).
					WillReturnRows(pgxmock.NewRows([]string{"actor_id", "virtual"}).AddRow(2, false))
				mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_configuration SET model = $1 WHERE actor_id = $2")).WithArgs("m", 2).
					WillReturnResult(pgxmock.NewResult("UPDATE", 1))
			},
			status: 200,
		},
		{
			name: "va-access revoke ownership check", path: "/admin/virtual-agent-access/revoke", body: `{"id":4}`,
			expect: func(mock pgxmock.PgxPoolIface) {
				grants(mock, 2, "agents", "write")
				mock.ExpectQuery(regexp.QuoteMeta("SELECT virtual_agent_id FROM virtual_agent_access WHERE id = $1")).WithArgs(boundNumber("4")).
					WillDelayFor(delayed).WillReturnRows(pgxmock.NewRows([]string{"virtual_agent_id"}).AddRow(2))
				mock.ExpectExec(regexp.QuoteMeta("DELETE FROM virtual_agent_access WHERE id = $1")).WithArgs(boundNumber("4")).
					WillReturnResult(pgxmock.NewResult("DELETE", 1))
			},
			status: 200,
		},
		{
			name: "change-password current password check", path: "/admin/change-password",
			body:   `{"current_password":"old password","new_password":"a long new password"}`,
			config: map[string]string{"minimum_password_length": "12"},
			expect: func(mock pgxmock.PgxPoolIface) {
				salt := "00ff"
				hash, _ := auth.Hash("old password", salt)
				mock.ExpectQuery(regexp.QuoteMeta("SELECT password_hash, password_salt FROM actors WHERE id = $1")).WithArgs(2).
					WillDelayFor(delayed).WillReturnRows(pgxmock.NewRows([]string{"password_hash", "password_salt"}).AddRow(&hash, &salt))
				mock.ExpectExec(regexp.QuoteMeta("UPDATE actors SET password_hash = $1, password_salt = $2 WHERE id = $3")).
					WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), 2).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
			},
			status: 200,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rt, mock, _ := newRoutes(t, c.config)
			c.expect(mock)
			rec := serveHungUp(t, rt, c.path, c.body)
			if rec.Code != c.status {
				t.Fatalf("got %d %s", rec.Code, rec.Body.String())
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
