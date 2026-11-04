package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v5"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// Vectors generated with the Node server's own code path
// (crypto.pbkdf2(token, saltHexString, 100000, 64, 'sha512'), sha256, and
// createHmac('sha256', secret)), so the Go hashes are proven to match the
// rows already in the database.
const (
	vectorSalt  = "5f1e0c9a2b7d4e6f8a1b3c5d7e9f0a2b4c6d8e0f1a3b5c7d9e1f3a5b7c9d1e3f"
	vectorToken = "Zm9vYmFyYmF6cXV4LXRlc3QtdG9rZW4tZm9yLWdvLXBvcnQtcGFyaXR5"
	vectorHash  = "36ce61920ec252e8a1c30a8ad033502f2410fad0d2dce210e6809994a854b9f650a38ed930f0f90b0aaa92c92ec01d12e66e8dcd34da41b9ad1d591f330e855e"
	vectorSHA   = "9227979f85a4a440dd38e8e629469169040ddfce86b05df3041d1bbe91b129ce"
	vectorHMAC  = "ab75dd616de750abeaaed49e2703f8d27bc9bdeabe9f137b1ea41e1556c37044"
)

func TestHashMatchesNode(t *testing.T) {
	got, err := Hash(vectorToken, vectorSalt)
	if err != nil || got != vectorHash {
		t.Fatalf("Hash = %s, %v", got, err)
	}
	if !Verify(vectorToken, vectorSalt, vectorHash) {
		t.Fatal("Verify rejected the Node hash")
	}
	if Verify("wrong", vectorSalt, vectorHash) {
		t.Fatal("Verify accepted a wrong token")
	}
	if Verify(vectorToken, vectorSalt, vectorHash[:10]) {
		t.Fatal("Verify accepted a truncated hash")
	}
	if Verify(vectorToken, vectorSalt, "not hex") {
		t.Fatal("Verify accepted a non-hex hash")
	}
	if TokenLookupHash(vectorToken) != vectorSHA {
		t.Fatalf("TokenLookupHash = %s", TokenLookupHash(vectorToken))
	}
	if HMACToken("test-secret", "work") != "work:"+vectorHMAC {
		t.Fatalf("HMACToken = %s", HMACToken("test-secret", "work"))
	}
}

var sessionCols = []string{"id", "actor_id", "token_hash", "token_salt", "expires_at", "name", "realms"}

func sessionRow(expires time.Time, actorID int, name string) *pgxmock.Rows {
	return pgxmock.NewRows(sessionCols).
		AddRow("8c1f6a52-0000-4000-8000-000000000001", actorID, vectorHash, vectorSalt, expires, name, []string{"llm-memory"})
}

var noSessions = func() *pgxmock.Rows { return pgxmock.NewRows(sessionCols) }

func TestValidateFastPathNoExtend(t *testing.T) {
	mock := testutil.NewMock(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs(SessionKindAPI, vectorSHA).
		WillReturnRows(sessionRow(now.Add(20*time.Hour), 4, "work"))

	s := &Sessions{DB: mock, Now: func() time.Time { return now }}
	row, err := s.Validate(context.Background(), vectorToken, SessionKindAPI)
	if err != nil || row == nil || row.ActorID != 4 || row.Name != "work" {
		t.Fatalf("Validate = %+v, %v", row, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateExtendsNearExpiry(t *testing.T) {
	mock := testutil.NewMock(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 123_456_000, time.UTC)
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs(SessionKindAPI, vectorSHA).
		WillReturnRows(sessionRow(now.Add(2*time.Hour), 4, "work"))
	want := now.Add(24 * time.Hour).Truncate(time.Millisecond)
	mock.ExpectExec("UPDATE sessions SET expires_at").WithArgs(want, "8c1f6a52-0000-4000-8000-000000000001").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := &Sessions{DB: mock, Now: func() time.Time { return now }}
	row, err := s.Validate(context.Background(), vectorToken, SessionKindAPI)
	if err != nil || row == nil || !row.ExpiresAt.Equal(want) {
		t.Fatalf("Validate = %+v, %v", row, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateLookupMatchWithBadHashFailsClosed(t *testing.T) {
	mock := testutil.NewMock(t)
	rows := pgxmock.NewRows(sessionCols).
		AddRow("id-1", 4, vectorHash, "different-salt", time.Now().Add(time.Hour), "work", []string{})
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs(SessionKindAPI, vectorSHA).WillReturnRows(rows)
	// No legacy query is expected: a failed verify must not fall through.

	s := &Sessions{DB: mock}
	row, err := s.Validate(context.Background(), vectorToken, SessionKindAPI)
	if err != nil || row != nil {
		t.Fatalf("Validate = %+v, %v; want nil", row, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateLegacyFallback(t *testing.T) {
	mock := testutil.NewMock(t)
	now := time.Now()
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs(SessionKindWeb, vectorSHA).WillReturnRows(noSessions())
	mock.ExpectQuery("token_lookup_hash IS NULL").WithArgs(SessionKindWeb).
		WillReturnRows(sessionRow(now.Add(20*time.Hour), 7, "jeff"))

	s := &Sessions{DB: mock, Now: func() time.Time { return now }}
	row, err := s.Validate(context.Background(), vectorToken, SessionKindWeb)
	if err != nil || row == nil || row.ActorID != 7 {
		t.Fatalf("Validate = %+v, %v", row, err)
	}
}

// --- /v1 session middleware ---

func newSessionAuth(mock pgxmock.PgxPoolIface, now func() time.Time) *SessionAuth {
	a := &SessionAuth{
		Sessions: &Sessions{DB: mock, Now: now},
		APIKeys:  &APIKeys{DB: mock},
	}
	a.cache.Now = now
	return a
}

func serveV1(a *SessionAuth, header string, path string) (*httptest.ResponseRecorder, *httpapi.State, bool) {
	var seen *httpapi.State
	reached := false
	h := httpapi.WithState(a.Middleware("/v1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		seen = httpapi.StateOf(r)
		w.WriteHeader(204)
	})))
	r := httptest.NewRequest("POST", path, nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec, seen, reached
}

func TestSessionAuthMissingHeader(t *testing.T) {
	rec, _, reached := serveV1(newSessionAuth(testutil.NewMock(t), nil), "", "/v1/mail/check")
	want := `{"error":{"code":"UNAUTHORIZED","message":"Missing Authorization header"}}`
	if reached || rec.Code != 401 || rec.Body.String() != want {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestSessionAuthUnauthenticatedPaths(t *testing.T) {
	a := newSessionAuth(testutil.NewMock(t), nil)
	for _, path := range []string{"/v1/agent/login", "/v1/admin/login"} {
		rec, _, reached := serveV1(a, "", path)
		if !reached || rec.Code != 204 {
			t.Fatalf("%s: reached=%v code=%d", path, reached, rec.Code)
		}
	}
}

func TestSessionAuthInvalidToken(t *testing.T) {
	mock := testutil.NewMock(t)
	// Not an API-key shape, so no key query runs; an actor with no mapping
	// would land here too, since every session query joins actors.
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs(SessionKindAPI, pgxmock.AnyArg()).WillReturnRows(noSessions())
	mock.ExpectQuery("token_lookup_hash IS NULL").WithArgs(SessionKindAPI).WillReturnRows(noSessions())
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs(SessionKindWeb, pgxmock.AnyArg()).WillReturnRows(noSessions())
	mock.ExpectQuery("token_lookup_hash IS NULL").WithArgs(SessionKindWeb).WillReturnRows(noSessions())

	rec, _, reached := serveV1(newSessionAuth(mock, nil), "Bearer nope", "/v1/mail/check")
	want := `{"error":{"code":"FORBIDDEN","message":"Invalid or expired session token"}}`
	if reached || rec.Code != 403 || rec.Body.String() != want {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionAuthAgentSessionIsCached(t *testing.T) {
	mock := testutil.NewMock(t)
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs(SessionKindAPI, vectorSHA).
		WillReturnRows(sessionRow(now.Add(20*time.Hour), 4, "work"))

	a := newSessionAuth(mock, clock)
	for i := 0; i < 2; i++ {
		rec, st, reached := serveV1(a, "Bearer "+vectorToken, "/v1/mail/check")
		if !reached || rec.Code != 204 {
			t.Fatalf("request %d: reached=%v code=%d", i, reached, rec.Code)
		}
		if st.AuthenticatedAgent != "work" || st.ActorID != 4 || st.AuthMethod != "session" || st.AuthenticatedUser != nil {
			t.Fatalf("request %d: state %+v", i, st)
		}
	}
	// One query for two requests: the second came from the cache.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// Past the cache TTL the token is validated again.
	now = now.Add(sessionCacheTTL + time.Second)
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs(SessionKindAPI, vectorSHA).
		WillReturnRows(sessionRow(now.Add(20*time.Hour), 4, "work"))
	serveV1(a, "Bearer "+vectorToken, "/v1/mail/check")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// Forget drops it at once.
	a.Forget(vectorToken)
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs(SessionKindAPI, vectorSHA).
		WillReturnRows(sessionRow(now.Add(20*time.Hour), 4, "work"))
	serveV1(a, "Bearer "+vectorToken, "/v1/mail/check")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionAuthWebSession(t *testing.T) {
	mock := testutil.NewMock(t)
	now := time.Now()
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs(SessionKindAPI, vectorSHA).WillReturnRows(noSessions())
	mock.ExpectQuery("token_lookup_hash IS NULL").WithArgs(SessionKindAPI).WillReturnRows(noSessions())
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs(SessionKindWeb, vectorSHA).
		WillReturnRows(sessionRow(now.Add(20*time.Hour), 7, "jeff"))

	_, st, reached := serveV1(newSessionAuth(mock, func() time.Time { return now }), "Bearer "+vectorToken, "/v1/admin/agents")
	if !reached || st.AuthenticatedUser == nil || st.AuthenticatedUser.Username != "jeff" || st.ActorID != 7 || st.AuthenticatedAgent != "" {
		t.Fatalf("reached=%v state=%+v", reached, st)
	}
}

func TestSessionAuthAPIKey(t *testing.T) {
	mock := testutil.NewMock(t)
	key := vectorHash[:64]
	keyHash, _ := Hash(key, vectorSalt)
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs(SessionKindAPI, pgxmock.AnyArg()).WillReturnRows(noSessions())
	mock.ExpectQuery("token_lookup_hash IS NULL").WithArgs(SessionKindAPI).WillReturnRows(noSessions())
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs(SessionKindWeb, pgxmock.AnyArg()).WillReturnRows(noSessions())
	mock.ExpectQuery("token_lookup_hash IS NULL").WithArgs(SessionKindWeb).WillReturnRows(noSessions())
	mock.ExpectQuery("key_lookup_hash = \\$1").WithArgs(TokenLookupHash(key)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "actor_id", "key_hash", "key_salt", "agent"}).
			AddRow(11, 21, keyHash, vectorSalt, "salem-engine"))
	mock.ExpectExec("UPDATE agent_api_keys SET last_used_at").WithArgs(11).WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	_, st, reached := serveV1(newSessionAuth(mock, nil), "Bearer "+key, "/v1/sim/soul")
	if !reached || st.AuthenticatedAgent != "salem-engine" || st.ActorID != 21 || st.AuthMethod != "api-key" {
		t.Fatalf("reached=%v state=%+v", reached, st)
	}
	testutil.WaitMet(t, mock)
}

func TestBearerPrefixRemovedAnywhere(t *testing.T) {
	mock := testutil.NewMock(t)
	now := time.Now()
	// "Token Bearer x" becomes "Token x", as header.replace('Bearer ', '') did.
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs(SessionKindAPI, TokenLookupHash("Token "+vectorToken)).WillReturnRows(noSessions())
	mock.ExpectQuery("token_lookup_hash IS NULL").WithArgs(SessionKindAPI).WillReturnRows(noSessions())
	mock.ExpectQuery("token_lookup_hash = \\$2").WithArgs(SessionKindWeb, TokenLookupHash("Token "+vectorToken)).WillReturnRows(noSessions())
	mock.ExpectQuery("token_lookup_hash IS NULL").WithArgs(SessionKindWeb).WillReturnRows(noSessions())
	serveV1(newSessionAuth(mock, func() time.Time { return now }), "Token Bearer "+vectorToken, "/v1/x")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatStampsAnyActor(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectExec("UPDATE actors SET last_seen").WithArgs(7).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	h := Heartbeat(mock, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	r := httpapi.WithStateValue(httptest.NewRequest("POST", "/v1/admin/agents", nil), &httpapi.State{ActorID: 7, AuthenticatedUser: &httpapi.User{ID: 7}})
	h.ServeHTTP(httptest.NewRecorder(), r)
	testutil.WaitMet(t, mock)

	// No actor, no write.
	h.ServeHTTP(httptest.NewRecorder(), httpapi.WithStateValue(httptest.NewRequest("POST", "/v1/agent/login", nil), &httpapi.State{}))
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// --- /mcp middleware ---

type recordingEvents struct {
	mu     sync.Mutex
	events []string
}

func (e *recordingEvents) Broadcast(event string, data any) {
	e.mu.Lock()
	e.events = append(e.events, event)
	e.mu.Unlock()
}

func newMCPAuth(t *testing.T, mock pgxmock.PgxPoolIface, configRows map[string]string) (*MCPAuth, *recordingEvents) {
	t.Helper()
	rows := pgxmock.NewRows([]string{"key", "value"})
	for k, v := range configRows {
		rows.AddRow(k, v)
	}
	mock.ExpectQuery("SELECT key, value FROM config").WillReturnRows(rows)
	store := config.NewStore()
	if err := store.Load(context.Background(), mock); err != nil {
		t.Fatal(err)
	}
	ev := &recordingEvents{}
	return &MCPAuth{
		DB:      mock,
		Config:  store,
		Actors:  actors.NewCache(mock),
		APIKeys: &APIKeys{DB: mock},
		Events:  ev,
		BaseURL: "https://memory.example.org",
	}, ev
}

var standardConfig = map[string]string{
	"mcp_oauth_bearer_secret": "test-secret",
	"signup_funnel_since":     "2026-10-01T00:00:00Z",
}

func serveMCP(m *MCPAuth, r *http.Request) (*httptest.ResponseRecorder, *httpapi.State, bool) {
	var seen *httpapi.State
	reached := false
	h := httpapi.WithState(m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		seen = httpapi.StateOf(r)
		w.WriteHeader(204)
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec, seen, reached
}

func mcpRequest(header string) *http.Request {
	r := httptest.NewRequest("POST", "/mcp", nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	return r
}

const challenge = `Bearer resource_metadata="https://memory.example.org/.well-known/oauth-protected-resource"`

func TestMCPAuthMissingToken(t *testing.T) {
	m, _ := newMCPAuth(t, testutil.NewMock(t), standardConfig)
	rec, _, reached := serveMCP(m, mcpRequest(""))
	want := `{"error":"unauthorized","error_description":"Missing or invalid Authorization header"}`
	if reached || rec.Code != 401 || rec.Body.String() != want || rec.Header().Get("WWW-Authenticate") != challenge {
		t.Fatalf("got %d %s %q", rec.Code, rec.Body.String(), rec.Header().Get("WWW-Authenticate"))
	}
}

func TestMCPAuthChallengeFromHost(t *testing.T) {
	m, _ := newMCPAuth(t, testutil.NewMock(t), standardConfig)
	m.BaseURL = ""
	r := mcpRequest("")
	r.Host = "memory.local:3100"
	rec, _, _ := serveMCP(m, r)
	if rec.Header().Get("WWW-Authenticate") != `Bearer resource_metadata="http://memory.local:3100/.well-known/oauth-protected-resource"` {
		t.Fatalf("challenge = %q", rec.Header().Get("WWW-Authenticate"))
	}
}

func TestMCPAuthValidHMAC(t *testing.T) {
	mock := testutil.NewMock(t)
	m, ev := newMCPAuth(t, mock, standardConfig)
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("work").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(4, "work"))
	mock.ExpectQuery("FROM agent_permissions").WithArgs(4).
		WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("notes").AddRow("mail"))
	since := "2026-10-01T00:00:00Z"
	mock.ExpectQuery("UPDATE actors").WithArgs(4, &since).
		WillReturnRows(pgxmock.NewRows([]string{"was_active"}).AddRow(true))

	rec, st, reached := serveMCP(m, mcpRequest("Bearer "+HMACToken("test-secret", "work")))
	if !reached || rec.Code != 204 {
		t.Fatalf("reached=%v code=%d body=%s", reached, rec.Code, rec.Body.String())
	}
	if st.MCPAgent != "work" || st.MCPActorID != 4 || len(st.MCPPermissions) != 2 {
		t.Fatalf("state %+v", st)
	}
	testutil.WaitMet(t, mock)
	deadline := time.Now().Add(time.Second)
	for {
		ev.mu.Lock()
		n := len(ev.events)
		ev.mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(ev.events) != 1 || ev.events[0] != "agent_activity" {
		t.Fatalf("events = %v", ev.events)
	}
}

func TestMCPAuthQueryToken(t *testing.T) {
	mock := testutil.NewMock(t)
	m, _ := newMCPAuth(t, mock, standardConfig)
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("work").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(4, "work"))
	mock.ExpectQuery("FROM agent_permissions").WithArgs(4).WillReturnRows(pgxmock.NewRows([]string{"name"}))
	mock.ExpectQuery("UPDATE actors").WithArgs(4, pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"was_active"}).AddRow(false))

	r := httptest.NewRequest("POST", "/mcp?token="+HMACToken("test-secret", "work"), nil)
	_, st, reached := serveMCP(m, r)
	if !reached || st.MCPAgent != "work" {
		t.Fatalf("reached=%v state=%+v", reached, st)
	}
	testutil.WaitMet(t, mock)
}

func TestMCPAuthForgedHMAC(t *testing.T) {
	mock := testutil.NewMock(t)
	m, _ := newMCPAuth(t, mock, standardConfig)
	forged := HMACToken("wrong-secret", "work")
	rec, _, reached := serveMCP(m, mcpRequest("Bearer "+forged))
	want := `{"error":"invalid_token","error_description":"Invalid token or API key"}`
	if reached || rec.Code != 401 || rec.Body.String() != want || rec.Header().Get("WWW-Authenticate") != challenge {
		t.Fatalf("got %d %s %q", rec.Code, rec.Body.String(), rec.Header().Get("WWW-Authenticate"))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMCPAuthHMACForUnknownAgent(t *testing.T) {
	mock := testutil.NewMock(t)
	m, _ := newMCPAuth(t, mock, standardConfig)
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("ghost").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}))
	rec, _, reached := serveMCP(m, mcpRequest("Bearer "+HMACToken("test-secret", "ghost")))
	want := `{"error":"invalid_token","error_description":"Agent not found in actors"}`
	if reached || rec.Code != 401 || rec.Body.String() != want || rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("got %d %s %q", rec.Code, rec.Body.String(), rec.Header().Get("WWW-Authenticate"))
	}
}

func TestMCPAuthBadAPIKeyHasNoChallenge(t *testing.T) {
	mock := testutil.NewMock(t)
	m, _ := newMCPAuth(t, mock, standardConfig)
	key := vectorHash[:64]
	mock.ExpectQuery("key_lookup_hash = \\$1").WillReturnRows(pgxmock.NewRows([]string{"id", "actor_id", "key_hash", "key_salt", "agent"}))
	mock.ExpectQuery("key_lookup_hash IS NULL").WillReturnRows(pgxmock.NewRows([]string{"id", "actor_id", "key_hash", "key_salt", "agent"}))

	rec, _, reached := serveMCP(m, mcpRequest("Bearer "+key))
	if reached || rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("got %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}

func TestMCPAuthValidAPIKey(t *testing.T) {
	mock := testutil.NewMock(t)
	m, _ := newMCPAuth(t, mock, standardConfig)
	key := vectorHash[:64]
	keyHash, _ := Hash(key, vectorSalt)
	mock.ExpectQuery("key_lookup_hash = \\$1").WithArgs(TokenLookupHash(key)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "actor_id", "key_hash", "key_salt", "agent"}).AddRow(11, 4, keyHash, vectorSalt, "work"))
	mock.ExpectExec("UPDATE agent_api_keys SET last_used_at").WithArgs(11).WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectQuery("FROM agent_permissions").WithArgs(4).WillReturnRows(pgxmock.NewRows([]string{"name"}).AddRow("notes"))
	mock.ExpectQuery("UPDATE actors").WithArgs(4, pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"was_active"}).AddRow(false))

	_, st, reached := serveMCP(m, mcpRequest("Bearer "+key))
	if !reached || st.MCPAgent != "work" || st.MCPActorID != 4 {
		t.Fatalf("reached=%v state=%+v", reached, st)
	}
	testutil.WaitMet(t, mock)
}

func TestMCPAuthMissingSecretIs500(t *testing.T) {
	m, _ := newMCPAuth(t, testutil.NewMock(t), map[string]string{"signup_funnel_since": ""})
	rec, _, reached := serveMCP(m, mcpRequest("Bearer work:abc"))
	want := `{"error":{"code":"INTERNAL_ERROR","message":"Config key \"mcp_oauth_bearer_secret\" not found. Was config.init() called?"}}`
	if reached || rec.Code != 500 || rec.Body.String() != want {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

// hookDB runs onQuery after each Query returns, standing in for work that
// happens between a lookup's database read and its cache write.
type hookDB struct {
	pgxmock.PgxPoolIface
	onQuery func()
}

func (h *hookDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	rows, err := h.PgxPoolIface.Query(ctx, sql, args...)
	if h.onQuery != nil {
		h.onQuery()
	}
	return rows, err
}

func TestLogoutDuringLookupIsNotUndone(t *testing.T) {
	mock := testutil.NewMock(t)
	now := time.Now()
	hooked := &hookDB{PgxPoolIface: mock}
	a := &SessionAuth{
		Sessions: &Sessions{DB: hooked, Now: func() time.Time { return now }},
		APIKeys:  &APIKeys{DB: hooked},
	}

	// Attempt 1 reads the live session; the logout lands right after.
	mock.ExpectQuery(`token_lookup_hash = \$2`).WithArgs(SessionKindAPI, vectorSHA).
		WillReturnRows(sessionRow(now.Add(20*time.Hour), 4, "work"))
	first := true
	hooked.onQuery = func() {
		if first {
			first = false
			a.Forget(vectorToken)
		}
	}
	// Attempt 2 finds the session gone.
	mock.ExpectQuery(`token_lookup_hash = \$2`).WithArgs(SessionKindAPI, vectorSHA).WillReturnRows(noSessions())
	mock.ExpectQuery("token_lookup_hash IS NULL").WithArgs(SessionKindAPI).WillReturnRows(noSessions())
	mock.ExpectQuery(`token_lookup_hash = \$2`).WithArgs(SessionKindWeb, vectorSHA).WillReturnRows(noSessions())
	mock.ExpectQuery("token_lookup_hash IS NULL").WithArgs(SessionKindWeb).WillReturnRows(noSessions())

	rec, _, reached := serveV1(a, "Bearer "+vectorToken, "/v1/mail/check")
	if reached || rec.Code != 403 {
		t.Fatalf("revoked token: reached=%v code=%d", reached, rec.Code)
	}
	if a.cache.Len() != 0 {
		t.Fatal("revoked session was cached")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRetryExhaustionFailsClosed(t *testing.T) {
	mock := testutil.NewMock(t)
	now := time.Now()
	hooked := &hookDB{PgxPoolIface: mock}
	a := &SessionAuth{
		Sessions: &Sessions{DB: hooked, Now: func() time.Time { return now }},
		APIKeys:  &APIKeys{DB: hooked},
	}
	// Every attempt reads a live session, and every attempt is invalidated.
	for i := 0; i < lookupAttempts; i++ {
		mock.ExpectQuery(`token_lookup_hash = \$2`).WithArgs(SessionKindAPI, vectorSHA).
			WillReturnRows(sessionRow(now.Add(20*time.Hour), 4, "work"))
	}
	hooked.onQuery = func() { a.Forget("some-other-token") }

	rec, _, reached := serveV1(a, "Bearer "+vectorToken, "/v1/mail/check")
	if reached || rec.Code != 403 {
		t.Fatalf("reached=%v code=%d", reached, rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
