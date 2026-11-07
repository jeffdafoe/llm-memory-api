package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v6"
)

func TestTemplatesAndAccessRequiredFields(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	cases := []struct {
		name string
		fn   func(*Routes) func(*httptest.ResponseRecorder, string) error
		body string
		want string
	}{
		{"list kind number", call((*Routes).templatesList), `{"kind":5}`, "Invalid kind: 5"},
		{"list kind array", call((*Routes).templatesList), `{"kind":["welcome","x"]}`, "Invalid kind: welcome,x"},
		{"read", call((*Routes).templatesRead), `{"id":0}`, "Required field: id"},
		{"save blank name", call((*Routes).templatesSave), `{"name":"\u0000","content":"c"}`, "Required fields: name, content"},
		{"save no content", call((*Routes).templatesSave), `{"name":"n"}`, "Required fields: name, content"},
		{"save kind", call((*Routes).templatesSave), `{"name":"n","content":"c","kind":"note"}`, "Invalid kind: note"},
		{"save no subject", call((*Routes).templatesSave), `{"name":"n","content":"---\nsubject: \n---\nB"}`,
			`Welcome templates require a subject in frontmatter (e.g. ---\nsubject: Welcome, {agent}\n---)`},
		{"delete", call((*Routes).templatesDelete), `{"id":""}`, "Required field: id"},
		{"approve", call((*Routes).accessRequestsApprove), `{}`, "Required field: id"},
		{"reject", call((*Routes).accessRequestsReject), `{"id":null}`, "Required field: id"},
		{"invite delete", call((*Routes).inviteCodesDelete), `{"id":false}`, "Required field: id"},
		{"expires zero", call((*Routes).inviteCodesGenerate), `{"expires_days":0}`, "expires_days must be a positive integer (max 3650)"},
		{"expires too long", call((*Routes).inviteCodesGenerate), `{"expires_days":"3651"}`, "expires_days must be a positive integer (max 3650)"},
		{"expires fraction", call((*Routes).inviteCodesGenerate), `{"expires_days":1.5}`, "expires_days must be a positive integer (max 3650)"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		if err := c.fn(rt)(rec, c.body); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		check(t, c.name, rec, 400, errorBody("BAD_REQUEST", c.want))
	}
	// A welcome template's content of another type fails where Node read
	// its header.
	if err := rt.templatesSave(httptest.NewRecorder(), request(`{"name":"n","content":5}`)); err == nil || err.Error() != "content must be a string" {
		t.Fatal(err)
	}
	for name, fn := range map[string]func(*Routes) func(*httptest.ResponseRecorder, string) error{
		"kind": call((*Routes).templatesList), "id": call((*Routes).templatesSave), "status": call((*Routes).accessRequestsList),
	} {
		err := fn(rt)(httptest.NewRecorder(), "")
		if err == nil || err.Error() != "Cannot destructure property '"+name+"' of 'req.body' as it is undefined." {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := rt.inviteCodesGenerate(httptest.NewRecorder(), request("")); err == nil || err.Error() != "Cannot read properties of undefined (reading 'count')" {
		t.Errorf("count: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A name of another type is refused after the update's 404 and the new
// name's 409, which Node answered before storing it.
func TestTemplatesSaveTypeOrder(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM templates WHERE id = $1")).WithArgs(boundNumber("9")).
		WillReturnRows(pgxmock.NewRows([]string{"?column?"}))
	rec := httptest.NewRecorder()
	if err := rt.templatesSave(rec, request(`{"id":9,"name":5,"content":"c","kind":"welcome-note"}`)); err != nil {
		t.Fatal(err)
	}
	check(t, "update missing", rec, 404, errorBody("NOT_FOUND", "Template not found"))

	mock.ExpectQuery(regexp.QuoteMeta("SELECT 1 FROM templates WHERE id = $1")).WithArgs(boundNumber("3")).
		WillReturnRows(pgxmock.NewRows([]string{"?column?"}).AddRow(1))
	if err := rt.templatesSave(httptest.NewRecorder(), request(`{"id":3,"name":5,"content":"c","kind":"welcome-note"}`)); err == nil || err.Error() != "name must be a string" {
		t.Fatal(err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM templates WHERE name = $1")).WithArgs(boundNumber("5")).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(4))
	rec = httptest.NewRecorder()
	if err := rt.templatesSave(rec, request(`{"name":5,"content":"c","kind":"welcome-note"}`)); err != nil {
		t.Fatal(err)
	}
	check(t, "duplicate", rec, 409, errorBody("CONFLICT", "A template with that name already exists"))

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM templates WHERE name = $1")).WithArgs("n").
		WillReturnRows(pgxmock.NewRows([]string{"id"}))
	if err := rt.templatesSave(httptest.NewRecorder(), request(`{"name":"n","description":{"a":1},"content":"c","kind":"welcome-note"}`)); err == nil || err.Error() != "description must be a string" {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAccessRequestReview(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	pending := func(id string, status string) {
		mock.ExpectQuery(regexp.QuoteMeta("SELECT status, email FROM access_requests WHERE id = $1")).WithArgs(boundNumber(id)).
			WillReturnRows(pgxmock.NewRows([]string{"status", "email"}).AddRow(status, "a@example.com"))
	}
	pending("1", "approved")
	rec := httptest.NewRecorder()
	if err := rt.accessRequestsReject(rec, request(`{"id":1}`)); err != nil {
		t.Fatal(err)
	}
	check(t, "already", rec, 400, errorBody("BAD_REQUEST", "Request already approved"))

	mock.ExpectQuery(regexp.QuoteMeta("SELECT status, email FROM access_requests WHERE id = $1")).WithArgs(boundNumber("2")).
		WillReturnRows(pgxmock.NewRows([]string{"status", "email"}))
	rec = httptest.NewRecorder()
	if err := rt.accessRequestsApprove(rec, request(`{"id":2}`)); err != nil {
		t.Fatal(err)
	}
	check(t, "missing", rec, 404, errorBody("NOT_FOUND", "Access request not found"))

	// notes of another type is refused after the pending check, before
	// any write.
	pending("3", "pending")
	if err := rt.accessRequestsApprove(httptest.NewRecorder(), request(`{"id":3,"notes":7}`)); err == nil || err.Error() != "notes must be a string" {
		t.Fatal(err)
	}

	pending("4", "pending")
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE access_requests SET status = 'approved'")).WithArgs("ok", boundNumber("4")).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT realms FROM actors WHERE id = $1")).WithArgs(2).
		WillReturnRows(pgxmock.NewRows([]string{"realms"}).AddRow([]string{}))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO invite_codes (code, created_by, access_request_id, expires_at, realm)")).
		WithArgs(pgxmock.AnyArg(), "admin", boundNumber("4"), "llm-memory").WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()
	mock.ExpectRollback()
	rt.BaseURL = "https://memory.example"
	rec = httptest.NewRecorder()
	if err := rt.accessRequestsApprove(rec, request(`{"id":4,"notes":"ok"}`)); err != nil {
		t.Fatal(err)
	}
	var body struct {
		OK          bool   `json:"ok"`
		InviteCode  string `json:"invite_code"`
		RegisterURL string `json:"register_url"`
		Email       string `json:"email"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if !body.OK || len(body.InviteCode) != 32 || body.RegisterURL != "https://memory.example/register?code="+body.InviteCode || body.Email != "a@example.com" {
		t.Fatalf("%s", rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInviteCodes(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	insert := func(days any, realm string) {
		mock.ExpectExec(regexp.QuoteMeta("INSERT INTO invite_codes (code, created_by, expires_at, realm)")).
			WithArgs(pgxmock.AnyArg(), "admin", days, realm).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	}
	// count is clamped to 1..50; a count that is not a safe integer is 1.
	insert(int64(7), "r")
	insert(int64(7), "r")
	rec := httptest.NewRecorder()
	if err := rt.inviteCodesGenerate(rec, request(`{"count":"2","expires_days":"7","realm":"r"}`)); err != nil {
		t.Fatal(err)
	}
	var body struct {
		OK    bool     `json:"ok"`
		Codes []string `json:"codes"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if !body.OK || len(body.Codes) != 2 || body.Codes[0] == body.Codes[1] {
		t.Fatalf("%s", rec.Body.String())
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT realms FROM actors WHERE id = $1")).WithArgs(2).
		WillReturnRows(pgxmock.NewRows([]string{"realms"}).AddRow([]string{"home", "x"}))
	insert(nil, "home")
	if err := rt.inviteCodesGenerate(httptest.NewRecorder(), request(`{"count":null,"expires_days":""}`)); err != nil {
		t.Fatal(err)
	}
	if err := rt.inviteCodesGenerate(httptest.NewRecorder(), request(`{"realm":5}`)); err == nil || err.Error() != "realm must be a string" {
		t.Fatal(err)
	}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT used_by FROM invite_codes WHERE id = $1")).WithArgs(boundNumber("5")).
		WillReturnRows(pgxmock.NewRows([]string{"used_by"}).AddRow(ptr("bob")))
	rec = httptest.NewRecorder()
	if err := rt.inviteCodesDelete(rec, request(`{"id":5}`)); err != nil {
		t.Fatal(err)
	}
	check(t, "used", rec, 400, errorBody("BAD_REQUEST", "Cannot delete a used invite code"))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func ptr(s string) *string { return &s }

// A client that hangs up during the pending or used check does not stop
// the write after it, as Node's pool queries ran on.
func TestAccessWritesOutliveTheRequest(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	delayedCheck := func(sql string, args, cols []any, names []string) {
		rows := pgxmock.NewRows(names)
		if cols != nil {
			rows.AddRow(cols...)
		}
		mock.ExpectQuery(regexp.QuoteMeta(sql)).WithArgs(args...).WillDelayFor(50 * time.Millisecond).WillReturnRows(rows)
	}
	// The request's context is cancelled while the check is in flight.
	run := func(name string, fn func(*Routes, http.ResponseWriter, *http.Request) error, body string) {
		req := request(body)
		ctx, cancel := context.WithCancel(req.Context())
		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()
		rec := httptest.NewRecorder()
		if err := fn(rt, rec, req.WithContext(ctx)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}

	delayedCheck("SELECT status, email FROM access_requests WHERE id = $1", []any{boundNumber("4")}, []any{"pending", "a@example.com"}, []string{"status", "email"})
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE access_requests SET status = 'approved'")).WithArgs(nil, boundNumber("4")).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT realms FROM actors WHERE id = $1")).WithArgs(2).
		WillReturnRows(pgxmock.NewRows([]string{"realms"}).AddRow([]string{"r"}))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO invite_codes (code, created_by, access_request_id, expires_at, realm)")).
		WithArgs(pgxmock.AnyArg(), "admin", boundNumber("4"), "r").WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectCommit()
	mock.ExpectRollback()
	run("approve", (*Routes).accessRequestsApprove, `{"id":4}`)

	delayedCheck("SELECT status, email FROM access_requests WHERE id = $1", []any{boundNumber("5")}, []any{"pending", "b@example.com"}, []string{"status", "email"})
	mock.ExpectExec(regexp.QuoteMeta("UPDATE access_requests SET status = 'rejected'")).WithArgs(nil, boundNumber("5")).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	run("reject", (*Routes).accessRequestsReject, `{"id":5}`)

	delayedCheck("SELECT used_by FROM invite_codes WHERE id = $1", []any{boundNumber("6")}, []any{(*string)(nil)}, []string{"used_by"})
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM invite_codes WHERE id = $1")).WithArgs(boundNumber("6")).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	run("invite delete", (*Routes).inviteCodesDelete, `{"id":6}`)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
