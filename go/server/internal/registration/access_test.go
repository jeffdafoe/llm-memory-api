package registration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v6"
)

func TestAccessRequestPending(t *testing.T) {
	rg := newRig(t, false)
	rg.mock.ExpectExec("INSERT INTO access_requests \\(email, usage_description\\) VALUES").
		WithArgs("a@example.com", "testing things").WillReturnResult(pgxmock.NewResult("INSERT", 1))
	status, body := rg.post("/api/access-request", `{"email":" a@example.com ","usage":"\ttesting things\n"}`)
	if status != 200 || len(body) != 1 || body["ok"] != true {
		t.Fatalf("%d %v", status, body)
	}
}

func TestAccessRequestAutoApproved(t *testing.T) {
	rg := newRig(t, true)
	var code any
	rg.mock.ExpectBegin()
	rg.mock.ExpectQuery("INSERT INTO access_requests .* 'approved', NOW\\(\\)\\) RETURNING id").
		WithArgs("a@example.com", "testing").WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(42))
	rg.mock.ExpectExec("INSERT INTO invite_codes .* 'system', \\$2, NOW\\(\\) \\+ INTERVAL '7 days'").
		WithArgs(capture{&code}, 42).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	rg.mock.ExpectCommit()
	status, body := rg.post("/api/access-request", `{"email":"a@example.com","usage":"testing"}`)
	if status != 200 || body["ok"] != true || body["auto_approved"] != true || body["code"] != code {
		t.Fatalf("%d %v (code %v)", status, body, code)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(code.(string)) {
		t.Fatalf("code %q", code)
	}
}

// A failed insert rolls the transaction back: no request is left approved
// without its invite.
func TestAccessRequestAutoApprovedRollsBack(t *testing.T) {
	rg := newRig(t, true)
	rg.mock.ExpectBegin()
	rg.mock.ExpectQuery("INSERT INTO access_requests").WithArgs("a@example.com", "testing").WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(42))
	rg.mock.ExpectExec("INSERT INTO invite_codes").WithArgs(pgxmock.AnyArg(), 42).WillReturnError(errors.New("boom"))
	rg.mock.ExpectRollback()
	status, body := rg.post("/api/access-request", `{"email":"a@example.com","usage":"testing"}`)
	if status != 500 || body["error"] != "Something went wrong" {
		t.Fatalf("%d %v", status, body)
	}
}

func TestAccessRequestValidation(t *testing.T) {
	long := strings.Repeat("x", 256)
	// 128 characters outside the BMP are 256 UTF-16 units.
	wide := strings.Repeat("\U0001F600", 128)
	cases := []struct {
		body   string
		status int
		want   string
	}{
		{`{}`, 400, "Email and usage description are required"},
		{`{"email":"a@example.com","usage":""}`, 400, "Email and usage description are required"},
		{`{"email":0,"usage":"x"}`, 400, "Email and usage description are required"},
		{`{"email":"` + long + `","usage":"x"}`, 400, "Input too long"},
		{`{"email":"` + wide + `","usage":"x"}`, 400, "Input too long"},
		{`{"email":"a","usage":"` + strings.Repeat("x", 5001) + `"}`, 400, "Input too long"},
		{`{"email":{"length":"300"},"usage":"x"}`, 400, "Input too long"},
		{`{"email":["a"],"usage":"x"}`, 500, "Something went wrong"},
		{`{"email":"a","usage":5}`, 500, "Something went wrong"},
		{`["email"]`, 400, "Email and usage description are required"},
	}
	for _, c := range cases {
		rg := newRig(t, false)
		status, body := rg.post("/api/access-request", c.body)
		if status != c.status || body["error"] != c.want {
			t.Errorf("%s: %d %v", c.body, status, body)
		}
	}
}

func TestAccessRequestWithoutJSONBody(t *testing.T) {
	rg := newRig(t, false)
	req := httptest.NewRequest(http.MethodPost, "/api/access-request", strings.NewReader("email=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	rg.h.ServeHTTP(rec, req)
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "Cannot destructure property 'email' of 'req.body' as it is undefined.") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// postHungUp sends an access request whose client hangs up 10ms in, while
// the first statement is still running.
func (rg *rig) postHungUp(body string) *httptest.ResponseRecorder {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(10*time.Millisecond, cancel)
	req := httptest.NewRequest(http.MethodPost, "/api/access-request", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rg.h.ServeHTTP(rec, req)
	return rec
}

// A client that hangs up does not stop the writes, as Node's queries ran on.
func TestAccessRequestWritesOutliveTheRequest(t *testing.T) {
	rg := newRig(t, false)
	rg.mock.ExpectExec("INSERT INTO access_requests").WithArgs("a@example.com", "testing").
		WillDelayFor(50 * time.Millisecond).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	if rec := rg.postHungUp(`{"email":"a@example.com","usage":"testing"}`); rec.Code != 200 {
		t.Fatalf("pending: %d %s", rec.Code, rec.Body.String())
	}

	rg = newRig(t, true)
	rg.mock.ExpectBegin()
	rg.mock.ExpectQuery("INSERT INTO access_requests").WithArgs("a@example.com", "testing").
		WillDelayFor(50 * time.Millisecond).WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(42))
	rg.mock.ExpectExec("INSERT INTO invite_codes").WithArgs(pgxmock.AnyArg(), 42).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	rg.mock.ExpectCommit()
	if rec := rg.postHungUp(`{"email":"a@example.com","usage":"testing"}`); rec.Code != 200 {
		t.Fatalf("auto-approved: %d %s", rec.Code, rec.Body.String())
	}
}
