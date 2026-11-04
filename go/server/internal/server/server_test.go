package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

func newHandler(t *testing.T) (http.Handler, pgxmock.PgxPoolIface) {
	t.Helper()
	mock := testutil.NewMock(t)
	return Handler(NewServices(mock, config.NewStore(), "")), mock
}

func TestHealth(t *testing.T) {
	h, mock := newHandler(t)
	for _, method := range []string{"GET", "HEAD"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, "/health", nil))
		if rec.Code != 200 {
			t.Fatalf("%s /health = %d", method, rec.Code)
		}
		if method == "GET" && rec.Body.String() != `{"status":"ok"}` {
			t.Fatalf("body = %s", rec.Body.String())
		}
		if rec.Header().Get("Content-Length") != "15" {
			t.Fatalf("%s content-length = %q", method, rec.Header().Get("Content-Length"))
		}
	}
	// /health is excluded from request_log, so no insert is expected.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownRouteIsExpress404AndLogged(t *testing.T) {
	h, mock := newHandler(t)
	mock.ExpectExec("INSERT INTO request_log").
		WithArgs("POST", "/nope?a=1", 404, pgxmock.AnyArg(), (*int)(nil), "192.0.2.1", (*int)(nil), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/nope?a=1", nil))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "<pre>Cannot POST /nope</pre>") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	testutil.WaitMet(t, mock)
}

func TestV1RequiresAuth(t *testing.T) {
	h, mock := newHandler(t)
	mock.ExpectExec("INSERT INTO request_log").WithArgs(
		"POST", "/v1/mail/check", 401, pgxmock.AnyArg(), (*int)(nil), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
	).WillReturnResult(pgxmock.NewResult("INSERT", 1))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/mail/check", nil))
	if rec.Code != 401 || rec.Body.String() != `{"error":{"code":"UNAUTHORIZED","message":"Missing Authorization header"}}` {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	testutil.WaitMet(t, mock)
}

func TestBadJSONIsAnsweredAndNotLogged(t *testing.T) {
	h, mock := newHandler(t)
	body := "{bad"
	r := httptest.NewRequest("POST", "/v1/mail/check", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != 400 || !strings.HasPrefix(rec.Body.String(), `{"error":{"code":"BAD_REQUEST"`) {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
