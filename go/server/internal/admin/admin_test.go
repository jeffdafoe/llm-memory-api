package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/permissions"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/providers"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/visibility"
)

// fakeCache records what logout drops from the token cache.
type fakeCache struct{ tokens []string }

func (f *fakeCache) Forget(token string) { f.tokens = append(f.tokens, token) }

var testNow = time.Date(2026, 11, 6, 12, 0, 0, 123456789, time.UTC)

func newRoutes(t *testing.T, values map[string]string) (*Routes, pgxmock.PgxPoolIface, *fakeCache) {
	mock := testutil.NewMock(t)
	cache := actors.NewCache(mock)
	log := logger.New(mock, cache, "")
	store := config.NewStore()
	for k, v := range values {
		store.Set(k, v)
	}
	adm := &permissions.Admin{DB: mock}
	tokens := &fakeCache{}
	rt := &Routes{
		Common: &Common{
			DB: mock, Log: log, Router: &httpapi.Router{Log: log}, Admin: adm, Actors: cache,
			Visibility: &visibility.Visibility{DB: mock, Admin: adm},
		},
		Config:      store,
		Sessions:    &auth.Sessions{DB: mock},
		SessionAuth: tokens,
		Providers:   &providers.Providers{Config: store, Log: log},
		Now:         func() time.Time { return testNow },
	}
	rt.Register(NewMux())
	return rt, mock, tokens
}

// request is an admin request from user 2 ("admin"); body "" is no JSON body.
func request(body string) *http.Request {
	r := httptest.NewRequest("POST", "/v1/admin/x", nil)
	r.Header.Set("Authorization", "Bearer web-token")
	st := &httpapi.State{ActorID: 2, AuthenticatedUser: &httpapi.User{ID: 2, Username: "admin"}}
	if body != "" {
		st.Body = json.RawMessage(body)
	}
	return httpapi.WithStateValue(r, st)
}

func errorBody(code, message string) string {
	m, _ := json.Marshal(message)
	return `{"error":{"code":"` + code + `","message":` + string(m) + `}}`
}

func check(t *testing.T, name string, rec *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if rec.Code != status || rec.Body.String() != body {
		t.Errorf("%s: got %d %s, want %d %s", name, rec.Code, rec.Body.String(), status, body)
	}
}

func grants(mock pgxmock.PgxPoolIface, actorID int, pairs ...string) {
	rows := pgxmock.NewRows([]string{"resource", "action"})
	for i := 0; i+1 < len(pairs); i += 2 {
		rows.AddRow(pairs[i], pairs[i+1])
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT resource, action FROM admin_permissions WHERE actor_id = $1")).
		WithArgs(actorID).WillReturnRows(rows)
}

// requireUser runs in front of every admin route: no admin web session is
// 401, except on the raw path /admin/login; with one, an unmatched path is
// the 404 page.
func TestHandlerRequireUser(t *testing.T) {
	mux := NewMux()
	mux.Handle("POST", "/admin/x", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	}))
	h := Handler(mux)
	cases := []struct {
		method, target string
		user           *httpapi.User
		status         int
	}{
		{"GET", "/v1/no-route", nil, 401},
		{"GET", "/v1/no-route", &httpapi.User{ID: 2, Username: "admin"}, 404},
		{"GET", "/v1/admin/login", nil, 404},
		{"GET", "/v1/agent/login", nil, 401},
		{"GET", "/v1/admin%2Flogin", nil, 401},
		{"POST", "/v1/admin/x", nil, 401},
		{"OPTIONS", "/v1/admin/x", nil, 401},
		{"OPTIONS", "/v1/admin/x", &httpapi.User{ID: 2}, 200},
		{"POST", "/v1/admin/x", &httpapi.User{ID: 2}, 204},
		{"GET", "/v1/admin/x", &httpapi.User{ID: 2}, 404},
	}
	for _, c := range cases {
		r := httpapi.WithStateValue(httptest.NewRequest(c.method, c.target, nil), &httpapi.State{AuthenticatedUser: c.user})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != c.status {
			t.Errorf("%s %s (user %v): %d %s", c.method, c.target, c.user != nil, rec.Code, rec.Body.String())
		}
		if c.status == 401 && rec.Body.String() != errorBody("UNAUTHORIZED", "Admin authentication required") {
			t.Errorf("%s: body %s", c.target, rec.Body.String())
		}
	}
}

func TestSafeInt(t *testing.T) {
	cases := []struct {
		raw string
		n   int64
		ok  bool
	}{
		{`7`, 7, true}, {`"7"`, 7, true}, {`" 7 "`, 7, true}, {`-3`, -3, true}, {`"0x10"`, 16, true},
		{`1e3`, 1000, true}, {`4.9`, 0, false}, {`"4abc"`, 0, false}, {`""`, 0, false}, {`"  "`, 0, false},
		{`null`, 0, false}, {``, 0, false}, {`true`, 0, false}, {`[7]`, 0, false}, {`{}`, 0, false},
		{`9007199254740992`, 0, false}, {`9007199254740991`, 9007199254740991, true},
	}
	for _, c := range cases {
		n, ok := SafeInt(json.RawMessage(c.raw))
		if n != c.n || ok != c.ok {
			t.Errorf("SafeInt(%s) = %d %v, want %d %v", c.raw, n, ok, c.n, c.ok)
		}
	}
}

func TestRequireOwnership(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	created := func(target any, by any) {
		rows := pgxmock.NewRows([]string{"created_by"})
		if by != false {
			rows.AddRow(by)
		}
		mock.ExpectQuery(regexp.QuoteMeta("SELECT created_by FROM actors WHERE id = $1")).WithArgs(target).WillReturnRows(rows)
	}
	grants(mock, 2, "agents", "write")

	// The caller itself passes without a query.
	rec := httptest.NewRecorder()
	if ok, err := rt.RequireOwnershipID(rec, request(`{}`), 2); !ok || err != nil {
		t.Fatalf("self: %v %v", ok, err)
	}
	created(5, 2)
	if ok, err := rt.RequireOwnershipID(httptest.NewRecorder(), request(`{}`), 5); !ok || err != nil {
		t.Fatalf("creator: %v %v", ok, err)
	}
	two := 9
	created(6, &two)
	rec = httptest.NewRecorder()
	if ok, err := rt.RequireOwnershipID(rec, request(`{}`), 6); ok || err != nil {
		t.Fatalf("other creator: %v %v", ok, err)
	}
	check(t, "other creator", rec, 403, errorBody("FORBIDDEN", "You can only modify agents you created"))
	created(7, false)
	rec = httptest.NewRecorder()
	if ok, _ := rt.RequireOwnershipID(rec, request(`{}`), 7); ok || rec.Code != 403 {
		t.Fatalf("missing actor: %v %d", ok, rec.Code)
	}
	// A string "2" is not strictly equal to 2, so it is looked up.
	created("2", 2)
	if ok, _ := rt.RequireOwnership(httptest.NewRecorder(), request(`{}`), "2", false); !ok {
		t.Fatal("string id of own creation")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// A superadmin passes without the created_by query.
	rt2, mock2, _ := newRoutes(t, nil)
	grants(mock2, 2, "*", "*")
	if ok, err := rt2.RequireOwnershipID(httptest.NewRecorder(), request(`{}`), 99); !ok || err != nil {
		t.Fatalf("superadmin: %v %v", ok, err)
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRequireVisibility(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, name FROM actors WHERE name = $1")).WithArgs("ghost").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}))
	rec := httptest.NewRecorder()
	if _, ok, err := rt.RequireVisibility(rec, request(`{}`), "ghost"); ok || err != nil {
		t.Fatalf("missing: %v %v", ok, err)
	}
	check(t, "missing", rec, 404, errorBody("NOT_FOUND", "Agent not found"))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLoginDirectAnswers(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	cases := []struct {
		name, body string
		status     int
		want       string
	}{
		{"no body", "", 500, errorBody("INTERNAL_ERROR", "Cannot read properties of undefined (reading 'username')")},
		{"empty", `{}`, 400, errorBody("BAD_REQUEST", "Required fields: username, password")},
		{"sanitized away", `{"username":"!!","password":"p"}`, 400, errorBody("BAD_REQUEST", "Required fields: username, password")},
		{"empty password", `{"username":"alice","password":""}`, 400, errorBody("BAD_REQUEST", "Required fields: username, password")},
		{"number username", `{"username":5,"password":"p"}`, 400, errorBody("BAD_REQUEST", "username must be a string")},
		{"array password", `{"username":"alice","password":["p"]}`, 400, errorBody("BAD_REQUEST", "password must be a string")},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		rt.login(rec, request(c.body))
		check(t, c.name, rec, c.status, c.want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func passwordRow(mock pgxmock.PgxPoolIface, name string, id int, password string) {
	salt := "00ff"
	hash, _ := auth.Hash(password, salt)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, name, password_hash, password_salt FROM actors WHERE name = $1 AND password_hash IS NOT NULL")).
		WithArgs(name).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "password_hash", "password_salt"}).AddRow(id, name, &hash, &salt))
}

func noPasswordRow(mock pgxmock.PgxPoolIface, name string) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, name, password_hash, password_salt FROM actors WHERE name = $1 AND password_hash IS NOT NULL")).
		WithArgs(name).WillReturnRows(pgxmock.NewRows([]string{"id", "name", "password_hash", "password_salt"}))
}

// admin-login.test.js: the right password creates a web session for that
// account; a wrong password, an unknown name and a passwordless account get
// the same 401 and no session.
func TestLogin(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	passwordRow(mock, "alice", 7, "right password")
	expires := testNow.Add(24 * time.Hour).Truncate(time.Millisecond)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO sessions (actor_id, token_hash, token_salt, token_lookup_hash, kind, expires_at) VALUES ($1, $2, $3, $4, $5, $6)")).
		WithArgs(7, pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), "web", expires).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE actors SET last_seen = NOW() WHERE id = $1")).WithArgs(7).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	grants(mock, 7, "dashboard", "read")

	rec := httptest.NewRecorder()
	rt.login(rec, request(`{"username":" Alice ","password":"right password"}`))
	var got struct {
		SessionToken string          `json:"session_token"`
		ExpiresAt    string          `json:"expires_at"`
		User         json.RawMessage `json:"user"`
		Permissions  json.RawMessage `json:"permissions"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if len(got.SessionToken) != 64 || got.ExpiresAt != "2026-11-07T12:00:00.123Z" ||
		string(got.User) != `{"id":7,"username":"alice"}` || string(got.Permissions) != `{"dashboard":["read"]}` {
		t.Fatalf("body %s", rec.Body.String())
	}
	if !strings.HasPrefix(rec.Body.String(), `{"session_token":`) {
		t.Fatalf("key order %s", rec.Body.String())
	}

	passwordRow(mock, "alice", 7, "right password")
	noPasswordRow(mock, "nobody")
	noPasswordRow(mock, "bot-without-password")
	for _, c := range [][2]string{{"alice", "wrong password"}, {"nobody", "right password"}, {"bot-without-password", "x"}} {
		rec := httptest.NewRecorder()
		rt.login(rec, request(`{"username":"`+c[0]+`","password":"`+c[1]+`"}`))
		check(t, c[0], rec, 401, errorBody("INVALID_CREDENTIALS", "Invalid username or password"))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLoginDatabaseError(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	mock.ExpectQuery("password_hash IS NOT NULL").WithArgs("alice").WillReturnError(errors.New("boom"))
	rec := httptest.NewRecorder()
	rt.login(rec, request(`{"username":"alice","password":"p"}`))
	check(t, "db error", rec, 500, errorBody("INTERNAL", "Login failed"))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLogout(t *testing.T) {
	rt, mock, tokens := newRoutes(t, nil)
	mock.ExpectQuery("token_lookup_hash = \\$3").WithArgs(2, "web", auth.TokenLookupHash("web-token")).
		WillReturnRows(pgxmock.NewRows([]string{"id", "token_hash", "token_salt"}))
	mock.ExpectQuery("token_lookup_hash IS NULL").WithArgs(2, "web").
		WillReturnRows(pgxmock.NewRows([]string{"id", "token_hash", "token_salt"}))
	rec := httptest.NewRecorder()
	rt.logout(rec, request(""))
	check(t, "logout", rec, 200, `{"message":"Logged out"}`)
	if len(tokens.tokens) != 1 || tokens.tokens[0] != "web-token" {
		t.Fatalf("forgot %v", tokens.tokens)
	}

	// A failed delete still drops the cached token.
	mock.ExpectQuery("token_lookup_hash = \\$3").WithArgs(2, "web", pgxmock.AnyArg()).WillReturnError(errors.New("down"))
	rec = httptest.NewRecorder()
	rt.logout(rec, request(""))
	check(t, "logout failure", rec, 500, errorBody("INTERNAL", "Logout failed"))
	if len(tokens.tokens) != 2 {
		t.Fatalf("forgot %v", tokens.tokens)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLinkAccount(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	cases := []struct {
		name, body string
		status     int
		want       string
	}{
		{"no body", "", 500, errorBody("INTERNAL", "Failed to link account")},
		{"empty", `{}`, 400, errorBody("BAD_REQUEST", "Required fields: username, password")},
		{"number password", `{"username":"bob","password":5}`, 400, errorBody("BAD_REQUEST", "password must be a string")},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		rt.linkAccount(rec, request(c.body))
		check(t, c.name, rec, c.status, c.want)
	}

	// Unknown target and wrong password: the same 400.
	noPasswordRow(mock, "nobody")
	rec := httptest.NewRecorder()
	rt.linkAccount(rec, request(`{"username":"nobody","password":"pw"}`))
	check(t, "unknown", rec, 400, errorBody("INVALID_CREDENTIALS", "Username or password is incorrect"))
	passwordRow(mock, "bob", 9, "bob-pw")
	rec = httptest.NewRecorder()
	rt.linkAccount(rec, request(`{"username":"bob","password":"wrong"}`))
	check(t, "wrong", rec, 400, errorBody("INVALID_CREDENTIALS", "Username or password is incorrect"))

	// Oneself.
	passwordRow(mock, "admin", 2, "admin-pw")
	rec = httptest.NewRecorder()
	rt.linkAccount(rec, request(`{"username":"admin","password":"admin-pw"}`))
	check(t, "self", rec, 400, errorBody("BAD_REQUEST", "That is the account you are logged in as"))

	// No shared realm.
	passwordRow(mock, "bob", 9, "bob-pw")
	realm := regexp.QuoteMeta("SELECT 1 FROM actors a1 JOIN actors a2 ON a1.realms && a2.realms WHERE a1.id = $1 AND a2.id = $2")
	mock.ExpectQuery(realm).WithArgs(2, 9).WillReturnRows(pgxmock.NewRows([]string{"?column?"}))
	rec = httptest.NewRecorder()
	rt.linkAccount(rec, request(`{"username":"bob","password":"bob-pw"}`))
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "NO_SHARED_REALM") {
		t.Fatalf("no realm: %d %s", rec.Code, rec.Body.String())
	}

	// The fifth attempt from this caller links; the sixth is throttled.
	passwordRow(mock, "bob", 9, "bob-pw")
	mock.ExpectQuery(realm).WithArgs(2, 9).WillReturnRows(pgxmock.NewRows([]string{"?column?"}).AddRow(1))
	mock.ExpectExec("INSERT INTO actor_visibility_configuration").WithArgs(2, 9).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	rec = httptest.NewRecorder()
	rt.linkAccount(rec, request(`{"username":"bob","password":"bob-pw"}`))
	check(t, "linked", rec, 200, `{"linked":"bob","already_linked":true}`)

	rec = httptest.NewRecorder()
	rt.linkAccount(rec, request(`{"username":"bob","password":"bob-pw"}`))
	if rec.Code != 429 || rec.Header().Get("Retry-After") != "900" ||
		rec.Body.String() != errorBody("RATE_LIMITED", "Too many attempts. Try again in 15 minutes.") {
		t.Fatalf("throttle: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestChangePassword(t *testing.T) {
	rt, mock, _ := newRoutes(t, map[string]string{"minimum_password_length": "12"})
	cases := []struct {
		name, body string
		status     int
		want       string
	}{
		{"no body", "", 500, errorBody("INTERNAL_ERROR", "Cannot destructure property 'current_password' of 'req.body' as it is undefined.")},
		{"missing", `{"current_password":"a"}`, 400, errorBody("BAD_REQUEST", "Required fields: current_password, new_password")},
		{"short", `{"current_password":"a","new_password":"short"}`, 400, errorBody("BAD_REQUEST", "Password must be at least 12 characters")},
		{"number", `{"current_password":"a","new_password":123456789012}`, 400, errorBody("BAD_REQUEST", "new_password must be a string")},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		rt.changePassword(rec, request(c.body))
		check(t, c.name, rec, c.status, c.want)
	}

	current := regexp.QuoteMeta("SELECT password_hash, password_salt FROM actors WHERE id = $1")
	salt := "00ff"
	hash, _ := auth.Hash("old password", salt)
	mock.ExpectQuery(current).WithArgs(2).WillReturnRows(pgxmock.NewRows([]string{"password_hash", "password_salt"}).AddRow(&hash, &salt))
	rec := httptest.NewRecorder()
	rt.changePassword(rec, request(`{"current_password":"wrong","new_password":"a long new password"}`))
	check(t, "wrong", rec, 400, errorBody("INVALID_CREDENTIALS", "Current password is incorrect"))

	// No dashboard password yet: the same 400, with no hashing of a NULL salt.
	mock.ExpectQuery(current).WithArgs(2).WillReturnRows(pgxmock.NewRows([]string{"password_hash", "password_salt"}).AddRow(nil, nil))
	rec = httptest.NewRecorder()
	rt.changePassword(rec, request(`{"current_password":"x","new_password":"a long new password"}`))
	check(t, "none", rec, 400, errorBody("INVALID_CREDENTIALS", "Current password is incorrect"))

	mock.ExpectQuery(current).WithArgs(2).WillReturnRows(pgxmock.NewRows([]string{"password_hash", "password_salt"}).AddRow(&hash, &salt))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE actors SET password_hash = $1, password_salt = $2 WHERE id = $3")).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), 2).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	rec = httptest.NewRecorder()
	rt.changePassword(rec, request(`{"current_password":"old password","new_password":"a long new password"}`))
	check(t, "changed", rec, 200, `{"message":"Password changed"}`)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProfileVisibility(t *testing.T) {
	rt, mock, _ := newRoutes(t, nil)
	if err := rt.profileVisibility(httptest.NewRecorder(), request("")); err == nil ||
		err.Error() != "Cannot read properties of undefined (reading 'actor_id')" {
		t.Fatalf("no body: %v", err)
	}
	read := regexp.QuoteMeta("SELECT visible_to_others FROM actors WHERE id = $1")
	mock.ExpectQuery(read).WithArgs(2).WillReturnRows(pgxmock.NewRows([]string{"visible_to_others"}).AddRow(true))
	rec := httptest.NewRecorder()
	if err := rt.profileVisibility(rec, request(`{}`)); err != nil {
		t.Fatal(err)
	}
	check(t, "read self", rec, 200, `{"visible_to_others":true}`)

	mock.ExpectQuery(read).WithArgs("9").WillReturnRows(pgxmock.NewRows([]string{"visible_to_others"}))
	rec = httptest.NewRecorder()
	rt.profileVisibility(rec, request(`{"actor_id":"9"}`))
	check(t, "read missing", rec, 200, `{"visible_to_others":false}`)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE actors SET visible_to_others = $1 WHERE id = $2")).WithArgs(false, jsutil.Number("9")).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	rec = httptest.NewRecorder()
	rt.profileVisibility(rec, request(`{"actor_id":9,"visible_to_others":null}`))
	check(t, "set", rec, 200, `{"visible_to_others":false}`)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestConfig(t *testing.T) {
	rt, mock, _ := newRoutes(t, map[string]string{"a": "old"})
	mock.ExpectQuery(regexp.QuoteMeta("SELECT key, value, description FROM config ORDER BY key")).
		WillReturnRows(pgxmock.NewRows([]string{"key", "value", "description"}).
			AddRow("a", strp("1"), nil).AddRow("mcp_oauth_bearer_secret", strp("s"), strp("secret")))
	rec := httptest.NewRecorder()
	if err := rt.configList(rec, request(`{}`)); err != nil {
		t.Fatal(err)
	}
	check(t, "list", rec, 200, `{"config":[{"key":"a","value":"1","description":null},{"key":"mcp_oauth_bearer_secret","value":"••••••••","description":"secret"}]}`)

	rec = httptest.NewRecorder()
	rt.configUpdate(rec, request(`{"value":"x"}`))
	check(t, "no key", rec, 400, errorBody("BAD_REQUEST", "Required field: key"))
	if err := rt.configUpdate(httptest.NewRecorder(), request(`{"key":"a","value":5}`)); err == nil || err.Error() != "value must be a string" {
		t.Fatalf("number value: %v", err)
	}

	update := regexp.QuoteMeta("UPDATE config SET value = $1 WHERE key = $2")
	mock.ExpectExec(update).WithArgs("", "nope").WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	rec = httptest.NewRecorder()
	rt.configUpdate(rec, request(`{"key":"nope"}`))
	check(t, "missing key", rec, 404, errorBody("NOT_FOUND", "Config key not found"))

	mock.ExpectExec(update).WithArgs("new", "a").WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	rec = httptest.NewRecorder()
	rt.configUpdate(rec, request(`{"key":"a","value":"new"}`))
	check(t, "update", rec, 200, `{"success":true}`)
	if v, _ := rt.Config.Get("a"); v != "new" {
		t.Fatalf("store has %q", v)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProvidersDefaults(t *testing.T) {
	rt, _, _ := newRoutes(t, nil)
	rec := httptest.NewRecorder()
	rt.providersDefaults(rec, request(`{"provider":"anthropic"}`))
	check(t, "missing model", rec, 400, errorBody("BAD_REQUEST", "provider and model are required"))
	rec = httptest.NewRecorder()
	if err := rt.providersDefaults(rec, request(`{"provider":"nope","model":"x"}`)); err != nil {
		t.Fatal(err)
	}
	check(t, "unknown model", rec, 200, `{"defaults":{}}`)
}

func TestVerify(t *testing.T) {
	mock := testutil.NewMock(t)
	log := logger.New(mock, actors.NewCache(mock), "")
	v := &Verify{Sessions: &auth.Sessions{DB: mock}, Admin: &permissions.Admin{DB: mock}, Log: log}
	mux := v.Mux(http.NotFoundHandler())

	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/auth/verify", nil)
		st := &httpapi.State{}
		if body != "" {
			st.Body = json.RawMessage(body)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httpapi.WithStateValue(r, st))
		return rec
	}
	check(t, "missing", post(`{}`), 400, `{"valid":false,"error":"Missing token"}`)
	check(t, "no body", post(""), 500, errorBody("INTERNAL_ERROR", "Cannot destructure property 'token' of 'req.body' as it is undefined."))
	check(t, "number", post(`{"token":5}`), 400, errorBody("BAD_REQUEST", "token must be a string"))

	salt := "00ff"
	hash, _ := auth.Hash("tok", salt)
	cols := []string{"id", "actor_id", "token_hash", "token_salt", "expires_at", "name", "realms"}
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs("web", auth.TokenLookupHash("tok")).
		WillReturnRows(pgxmock.NewRows(cols))
	mock.ExpectQuery("token_lookup_hash IS NULL").WithArgs("web").WillReturnRows(pgxmock.NewRows(cols))
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs("api", auth.TokenLookupHash("tok")).
		WillReturnRows(pgxmock.NewRows(cols).AddRow("s1", 4, hash, salt, testNow.Add(48*time.Hour), "agent-a", nil))
	grants(mock, 4)
	check(t, "api session", post(`{"token":"tok"}`), 200,
		`{"valid":true,"agent":"agent-a","actor_id":4,"realms":[],"session_kind":"api","permissions":{}}`)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("OPTIONS", "/v1/auth/verify", nil))
	check(t, "options", rec, 200, "POST")
}

func strp(s string) *string { return &s }
