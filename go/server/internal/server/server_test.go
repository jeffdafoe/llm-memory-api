package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
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

func TestPanicOutsideRouteIsLoggedAs500(t *testing.T) {
	mock := testutil.NewMock(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) { panic("boom") })
	requestLog := &httpapi.RequestLog{DB: mock}
	h := httpapi.Recover(httpapi.WithState(httpapi.ParseJSONBody(requestLog.Middleware(httpapi.Recover(mux)))))

	mock.ExpectExec("INSERT INTO request_log").WithArgs(
		"POST", "/boom", 500, pgxmock.AnyArg(), (*int)(nil), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
	).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/boom", nil))
	if rec.Code != 500 || rec.Body.String() != `{"error":{"code":"INTERNAL_ERROR","message":"boom"}}` {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	testutil.WaitMet(t, mock)
}

// Express never cleans or redirects a path, and mounts /v1 for "/v1" too.
func TestPathsExpressDoesNotRedirect(t *testing.T) {
	cases := []struct {
		target string
		status int
		body   string
	}{
		{"//double//slash", 404, "<pre>Cannot GET //double//slash</pre>"},
		{"/./health", 404, "<pre>Cannot GET /./health</pre>"},
		{"/v1", 401, `{"error":{"code":"UNAUTHORIZED","message":"Missing Authorization header"}}`},
		{"/v1//x", 401, `{"error":{"code":"UNAUTHORIZED","message":"Missing Authorization header"}}`},
		{"/v1/../health", 401, `{"error":{"code":"UNAUTHORIZED","message":"Missing Authorization header"}}`},
		// The login exemption is on the raw path, as Express's req.path.
		{"/v1/agent%2Flogin", 401, `{"error":{"code":"UNAUTHORIZED","message":"Missing Authorization header"}}`},
		// The top level is decided on the raw path: these decode to /v1/...
		// and /health but are neither in Express.
		{"/%76%31/missing", 404, "<pre>Cannot GET /%76%31/missing</pre>"},
		{"/v%31/missing", 404, "<pre>Cannot GET /v%31/missing</pre>"},
		{"/v1%2Fmissing", 404, "<pre>Cannot GET /v1%2Fmissing</pre>"},
		{"/%68ealth", 404, "<pre>Cannot GET /%68ealth</pre>"},
		// ServeMux cleans the escaped path, so an encoded dot segment is not
		// a dot segment; it reaches the /v1 chain like any other path.
		{"/v1/%2e%2e/x", 401, `{"error":{"code":"UNAUTHORIZED","message":"Missing Authorization header"}}`},
	}
	for _, c := range cases {
		h, mock := newHandler(t)
		mock.ExpectExec("INSERT INTO request_log").WithArgs(
			"GET", c.target, c.status, pgxmock.AnyArg(), (*int)(nil), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
		).WillReturnResult(pgxmock.NewResult("INSERT", 1))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", c.target, nil))
		if rec.Code != c.status || !strings.Contains(rec.Body.String(), c.body) {
			t.Errorf("%s: got %d %s", c.target, rec.Code, rec.Body.String())
		}
		testutil.WaitMet(t, mock)
	}
}

// The /v1 fallback is Node's requireUser: 401 without an admin web session,
// except on /admin/login; the 404 page with one.
func TestV1Unmatched(t *testing.T) {
	cases := []struct {
		target string
		user   *httpapi.User
		status int
	}{
		{"/v1/no-route", nil, 401},
		{"/v1/no-route", &httpapi.User{ID: 2, Username: "admin"}, 404},
		{"/v1/admin/login", nil, 404},
		{"/v1/agent/login", nil, 401},
		{"/v1/admin%2Flogin", nil, 401},
	}
	for _, c := range cases {
		r := httpapi.WithStateValue(httptest.NewRequest("GET", c.target, nil), &httpapi.State{AuthenticatedUser: c.user})
		rec := httptest.NewRecorder()
		v1Unmatched(rec, r)
		if rec.Code != c.status {
			t.Errorf("%s (user %v): %d %s", c.target, c.user != nil, rec.Code, rec.Body.String())
		}
		if c.status == 401 && rec.Body.String() != `{"error":{"code":"UNAUTHORIZED","message":"Admin authentication required"}}` {
			t.Errorf("%s: body %s", c.target, rec.Body.String())
		}
	}
}
