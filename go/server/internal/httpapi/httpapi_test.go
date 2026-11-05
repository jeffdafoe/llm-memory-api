package httpapi

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// The expected bodies below were captured from the Node server's Express 5
// stack (express.json + the server.js error handler + finalhandler).

func anyArgs(n int) []any {
	args := make([]any, n)
	for i := range args {
		args[i] = pgxmock.AnyArg()
	}
	return args
}

func TestRouteClientErrorBody(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectExec("INSERT INTO error_log").WithArgs(anyArgs(8)...).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	rt := &Router{Log: logger.New(mock, actors.NewCache(mock), "")}

	h := rt.Route("mail", "mail-send", func(w http.ResponseWriter, r *http.Request) error {
		return apierr.New(http.StatusConflict, `Mail "x" was already read`)
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/v1/mail/send", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d", rec.Code)
	}
	want := `{"error":{"code":"CONFLICT","message":"Mail \"x\" was already read"}}`
	if rec.Body.String() != want {
		t.Fatalf("body = %s, want %s", rec.Body.String(), want)
	}
	if rec.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("content-type = %q", rec.Header().Get("Content-Type"))
	}
	testutil.WaitMet(t, mock)
}

func TestRouteCustomCode(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectExec("INSERT INTO error_log").WithArgs(anyArgs(8)...).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	rt := &Router{Log: logger.New(mock, actors.NewCache(mock), "")}

	h := rt.Route("x", "y", func(w http.ResponseWriter, r *http.Request) error {
		return apierr.WithCode(http.StatusBadRequest, "INVALID_CREDENTIALS", "Invalid credentials")
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/", nil))
	want := `{"error":{"code":"INVALID_CREDENTIALS","message":"Invalid credentials"}}`
	if rec.Code != 400 || rec.Body.String() != want {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	testutil.WaitMet(t, mock)
}

func TestRouteServerErrorHidesMessage(t *testing.T) {
	mock := testutil.NewMock(t)
	// The internal message goes to error_log with status 500; the response
	// carries only the label.
	status := 500
	mock.ExpectExec("INSERT INTO error_log").
		WithArgs("documents", "save-note", (*int)(nil), (*string)(nil), (*string)(nil), "connection refused", pgxmock.AnyArg(), &status).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	rt := &Router{Log: logger.New(mock, actors.NewCache(mock), "")}

	h := rt.Route("documents", "save-note", func(w http.ResponseWriter, r *http.Request) error {
		return errors.New("connection refused")
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/", nil))
	want := `{"error":{"code":"INTERNAL","message":"Failed: save-note"}}`
	if rec.Code != 500 || rec.Body.String() != want {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	testutil.WaitMet(t, mock)
}

func TestRoutePanicIsA500(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectExec("INSERT INTO error_log").WithArgs(anyArgs(8)...).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	rt := &Router{Log: logger.New(mock, actors.NewCache(mock), "")}

	h := rt.Route("x", "boom", func(w http.ResponseWriter, r *http.Request) error {
		var m map[string]int
		m["x"] = 1
		return nil
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/", nil))
	if rec.Code != 500 || rec.Body.String() != `{"error":{"code":"INTERNAL","message":"Failed: boom"}}` {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	testutil.WaitMet(t, mock)
}

func TestMarshalDoesNotEscapeHTML(t *testing.T) {
	got, err := WriteJSONString(map[string]string{"a": "<b>&"})
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"a":"<b>&"}` {
		t.Fatalf("got %s", got)
	}
}

func WriteJSONString(v any) (string, error) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, 200, v)
	return rec.Body.String(), nil
}

func bodyRequest(contentType, body string, headers map[string]string) *http.Request {
	r := httptest.NewRequest("POST", "/health", strings.NewReader(body))
	// httptest does not set the header; a real server request carries it.
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func serveBody(r *http.Request) (*httptest.ResponseRecorder, *State) {
	var seen *State
	h := WithState(ParseJSONBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = StateOf(r)
		w.WriteHeader(204)
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec, seen
}

func TestParseJSONBodyRejections(t *testing.T) {
	cases := []struct {
		name    string
		req     *http.Request
		status  int
		body    string
		exactly bool
	}{
		{
			name:    "charset",
			req:     bodyRequest("application/json; charset=latin1", "{}", nil),
			status:  415,
			body:    `{"error":{"code":"BAD_REQUEST","message":"unsupported charset \"LATIN1\""}}`,
			exactly: true,
		},
		{
			name:    "encoding",
			req:     bodyRequest("application/json", "{}", map[string]string{"Content-Encoding": "zz"}),
			status:  415,
			body:    `{"error":{"code":"BAD_REQUEST","message":"unsupported content encoding \"zz\""}}`,
			exactly: true,
		},
		{
			name:    "too large",
			req:     bodyRequest("application/json", `{"a":"`+strings.Repeat("x", MaxBodyBytes)+`"}`, nil),
			status:  413,
			body:    `{"error":{"code":"BAD_REQUEST","message":"request entity too large"}}`,
			exactly: true,
		},
		{
			// V8's JSON.parse message text cannot be reproduced; only the
			// status and code are kept.
			name:   "malformed",
			req:    bodyRequest("application/json", "{bad", nil),
			status: 400,
			body:   `{"error":{"code":"BAD_REQUEST","message":`,
		},
		{
			name:   "strict mode",
			req:    bodyRequest("application/json", `"str"`, nil),
			status: 400,
			body:   `{"error":{"code":"BAD_REQUEST","message":`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, seen := serveBody(tc.req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if seen != nil {
				t.Fatal("next handler ran for a rejected body")
			}
			if tc.exactly && rec.Body.String() != tc.body {
				t.Fatalf("body = %s, want %s", rec.Body.String(), tc.body)
			}
			if !tc.exactly && !strings.HasPrefix(rec.Body.String(), tc.body) {
				t.Fatalf("body = %s, want prefix %s", rec.Body.String(), tc.body)
			}
		})
	}
}

func TestParseJSONBodyAccepts(t *testing.T) {
	rec, seen := serveBody(bodyRequest("application/json; charset=utf-8", `{"a":1}`, nil))
	if rec.Code != 204 || string(seen.Body) != `{"a":1}` {
		t.Fatalf("got %d %s", rec.Code, seen.Body)
	}

	rec, seen = serveBody(bodyRequest("application/json", "", nil))
	if rec.Code != 204 || string(seen.Body) != "{}" {
		t.Fatalf("empty body: got %d %q", rec.Code, seen.Body)
	}

	rec, seen = serveBody(bodyRequest("text/plain", "not json", nil))
	if rec.Code != 204 || seen.Body != nil {
		t.Fatalf("text/plain: got %d %q", rec.Code, seen.Body)
	}

	noBody := httptest.NewRequest("POST", "/health", nil)
	noBody.Header.Set("Content-Type", "application/json")
	noBody.Header.Del("Content-Length")
	rec, seen = serveBody(noBody)
	if rec.Code != 204 || seen.Body != nil {
		t.Fatalf("no body: got %d %q", rec.Code, seen.Body)
	}
}

func TestNotFoundMatchesExpress(t *testing.T) {
	cases := []struct {
		method, target, want string
	}{
		{"GET", "/v1/nope?x=<b>&y=1", "<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<title>Error</title>\n</head>\n<body>\n<pre>Cannot GET /v1/nope</pre>\n</body>\n</html>\n"},
		{"POST", "/a%20b/%3Cx%3E", "<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<title>Error</title>\n</head>\n<body>\n<pre>Cannot POST /a%20b/%3Cx%3E</pre>\n</body>\n</html>\n"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		NotFound(rec, httptest.NewRequest(tc.method, tc.target, nil))
		if rec.Code != 404 || rec.Body.String() != tc.want {
			t.Fatalf("%s %s: got %d %q", tc.method, tc.target, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Content-Security-Policy") != "default-src 'none'" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("headers = %v", rec.Header())
		}
	}
}

func TestClientAddress(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	if got := ClientAddress(r); got != "127.0.0.1" {
		t.Fatalf("no XFF: %q", got)
	}
	r.Header.Set("X-Forwarded-For", "127.0.0.1, 203.0.113.9")
	if got := ClientAddress(r); got != "203.0.113.9" {
		t.Fatalf("spoofed first hop: %q", got)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.9, ")
	if got := ClientAddress(r); got != "127.0.0.1" {
		t.Fatalf("empty last hop: %q", got)
	}
}

func TestRequestLogRow(t *testing.T) {
	mock := testutil.NewMock(t)
	body := `{"method":"tools/call","params":{"name":"save_note"}}`
	actorID := 4
	reqLen := len(body) + 2
	respLen := 2
	mock.ExpectExec("INSERT INTO request_log").
		WithArgs("POST", "/mcp → save_note", 200, pgxmock.AnyArg(), &actorID, "198.51.100.7", &reqLen, &respLen).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	rl := &RequestLog{DB: mock}
	h := WithState(ParseJSONBody(rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		StateOf(r).MCPActorID = 4
		w.Write([]byte("ok"))
	}))))
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body+"  "))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)+2))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	h.ServeHTTP(httptest.NewRecorder(), r)
	testutil.WaitMet(t, mock)
}

func TestRequestLogSkipsExcludedPaths(t *testing.T) {
	mock := testutil.NewMock(t)
	rl := &RequestLog{DB: mock}
	h := WithState(rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, bytes.NewReader(nil))
	})))
	for _, path := range []string{"/health", "/admin/index.html", "/v1/admin/dashboard?x=1"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMCPDisplayPath(t *testing.T) {
	cases := map[string]string{
		`{"method":"tools/call","params":{"name":"read_note"}}`: "/mcp → read_note",
		`{"method":"tools/call","params":{}}`:                   "/mcp → tools/call",
		`{"method":"initialize"}`:                               "/mcp → initialize",
		`{"method":""}`:                                         "/mcp",
		`[{"method":"initialize"}]`:                             "/mcp",
	}
	for body, want := range cases {
		if got := mcpDisplayPath([]byte(body), "/mcp"); got != want {
			t.Errorf("%s: got %q, want %q", body, got, want)
		}
	}
	if got := mcpDisplayPath(nil, "/mcp"); got != "/mcp" {
		t.Errorf("nil body: %q", got)
	}
}

// Cases captured from the Express 5 probe (express.json, body-parser).
func TestParseJSONBodyCharsets(t *testing.T) {
	utf16le := func(s string) string {
		var b []byte
		for _, r := range s {
			b = append(b, byte(r), byte(r>>8))
		}
		return string(b)
	}
	utf16be := func(s string) string {
		var b []byte
		for _, r := range s {
			b = append(b, byte(r>>8), byte(r))
		}
		return string(b)
	}
	cases := []struct {
		name, contentType, body string
		status                  int
		want                    string
	}{
		{"utf-8 BOM", "application/json", "\xef\xbb\xbf{\"a\":1}", 204, `{"a":1}`},
		{"utf-16le", "application/json; charset=utf-16le", utf16le(`{"a":"é"}`), 204, `{"a":"é"}`},
		{"utf-16 with BOM", "application/json; charset=utf-16", "\xff\xfe" + utf16le(`{"a":2}`), 204, `{"a":2}`},
		{"utf-16 without BOM is LE", "application/json; charset=utf-16", utf16le(`{"a":3}`), 204, `{"a":3}`},
		{"utf-16be", "application/json; charset=utf-16be", utf16be(`{"a":4}`), 204, `{"a":4}`},
		{"invalid utf-8 becomes U+FFFD", "application/json", "{\"a\":\"\xff\"}", 204, "{\"a\":\"\uFFFD\"}"},
		{"whitespace only", "application/json", "   \n", 400, `{"error":{"code":"BAD_REQUEST","message":"Unexpected end of JSON input"}}`},
		{"unknown utf charset", "application/json; charset=utf-9", "{}", 415, `{"error":{"code":"BAD_REQUEST","message":"unsupported charset \"UTF-9\""}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, seen := serveBody(bodyRequest(tc.contentType, tc.body, nil))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			if tc.status == 204 {
				if string(seen.Body) != tc.want {
					t.Fatalf("body = %q, want %q", seen.Body, tc.want)
				}
				return
			}
			if rec.Body.String() != tc.want {
				t.Fatalf("response = %s, want %s", rec.Body.String(), tc.want)
			}
		})
	}
}

func TestRecorderStatus(t *testing.T) {
	// A flush commits 200; a later 500 is not what the client got.
	rec := &recordingWriter{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	rec.Flush()
	rec.WriteHeader(500)
	if rec.status != 200 {
		t.Fatalf("after flush: status = %d", rec.status)
	}

	// An informational status is not the final one.
	rec = &recordingWriter{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	rec.WriteHeader(http.StatusEarlyHints)
	rec.WriteHeader(201)
	if rec.status != 201 {
		t.Fatalf("after 103: status = %d", rec.status)
	}

	// 101 is final.
	rec = &recordingWriter{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	rec.WriteHeader(http.StatusSwitchingProtocols)
	rec.WriteHeader(200)
	if rec.status != 101 {
		t.Fatalf("after 101: status = %d", rec.status)
	}
}

func TestRouteEncodeErrorIsA500(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectExec("INSERT INTO error_log").WithArgs(anyArgs(8)...).WillReturnResult(pgxmock.NewResult("INSERT", 1))
	rt := &Router{Log: logger.New(mock, actors.NewCache(mock), "")}

	h := rt.Route("x", "encode", func(w http.ResponseWriter, r *http.Request) error {
		return WriteJSON(w, 200, map[string]any{"bad": make(chan int)})
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest("POST", "/", nil))
	if rec.Code != 500 || rec.Body.String() != `{"error":{"code":"INTERNAL","message":"Failed: encode"}}` {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	testutil.WaitMet(t, mock)
}

// Expected outputs are Node's Buffer.toString('utf8') of the same bytes.
func TestReplaceInvalidUTF8MatchesNode(t *testing.T) {
	const r = "\xef\xbf\xbd"
	cases := []struct {
		name     string
		in, want string
	}{
		{"truncated 3-byte before quote", "\"\xe2\x82\"", "\"" + r + "\""},
		{"truncated 2-byte", "\"\xc3\"", "\"" + r + "\""},
		{"truncated 4-byte, 3 of 4", "\"\xf0\x9f\x98\"", "\"" + r + "\""},
		{"truncated 4-byte, 2 of 4", "\"\xf0\x9f\"", "\"" + r + "\""},
		{"ff", "\"\xff\"", "\"" + r + "\""},
		{"overlong 2-byte", "\"\xc0\x80\"", "\"" + r + r + "\""},
		{"overlong 3-byte", "\"\xe0\x80\x80\"", "\"" + r + r + r + "\""},
		{"surrogate", "\"\xed\xa0\x80\"", "\"" + r + r + r + "\""},
		{"above U+10FFFF", "\"\xf4\x90\x80\x80\"", "\"" + r + r + r + r + "\""},
		{"truncated then ASCII", "\"\xe2\x82A\"", "\"" + r + "A\""},
		{"lone continuations", "\"\x80\x80\"", "\"" + r + r + "\""},
		{"valid stays", "\"é😀\"", "\"é😀\""},
	}
	for _, tc := range cases {
		if got := string(replaceInvalidUTF8([]byte(tc.in))); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
