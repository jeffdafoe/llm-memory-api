package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/auth"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/db"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/logger"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// Ported from node/api/src/routes/oauth.test.js and
// oauth-public-client.test.js. The database is pgxmock; the API-key lookup
// and the password check are stubbed as the Node tests stubbed them.

const (
	testSecret       = "test-secret"
	password         = "correct horse battery"
	claudeCallback   = "https://claude.ai/api/mcp/auth_callback"
	claudeCOMCallbck = "https://claude.com/api/mcp/auth_callback"
	cimdClientID     = "https://claude.ai/oauth/mcp-oauth-client-metadata"
)

type fakeKeys struct{}

func (fakeKeys) Find(ctx context.Context, token string, actorID int) (*auth.APIKeyMatch, error) {
	if token == "good-secret" {
		return &auth.APIKeyMatch{ID: 1, ActorID: actorID}, nil
	}
	return nil, nil
}

// passwords are the agents that have a password.
var passwords = map[string]int{"alice": 7, "bob": 8, "carol": 9}

func fakeVerify(ctx context.Context, conn db.DB, username, pw string) (*auth.PasswordAccount, error) {
	id, ok := passwords[username]
	if !ok || pw != password {
		return nil, nil
	}
	return &auth.PasswordAccount{ID: id, Name: username}, nil
}

type harness struct {
	t    *testing.T
	rt   *Routes
	mock pgxmock.PgxPoolIface
	h    http.Handler
}

func newHarness(t *testing.T) *harness {
	mock := testutil.NewMock(t)
	store := config.NewStore()
	store.Set("mcp_oauth_bearer_secret", testSecret)
	cache := actors.NewCache(mock)
	rt := New(mock, store, cache, fakeKeys{}, logger.New(mock, cache, ""), "")
	rt.verifyPassword = fakeVerify
	mux := httpapi.NewRawMux("", http.HandlerFunc(httpapi.NotFound))
	rt.Register(mux)
	return &harness{t: t, rt: rt, mock: mock, h: httpapi.WithState(httpapi.ParseJSONBody(mux))}
}

// expectAgent sets up one /authorize agent-name pass: the actor lookup (the
// first time only; it is cached) and the agent row.
func (hs *harness) expectAgent(name string, firstLookup bool) {
	if firstLookup {
		hs.mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs(name).
			WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(1, name))
	}
	hs.mock.ExpectQuery("SELECT ac.name AS agent, ac.status").WithArgs(1).
		WillReturnRows(pgxmock.NewRows([]string{"agent", "status"}).AddRow(name, "active"))
}

func (hs *harness) expectNoActor(name string) {
	hs.mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs(name).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}))
}

func (hs *harness) do(method, target string, header map[string]string, body string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	return rec
}

func pkce() (verifier, challenge string) {
	b := make([]byte, 32)
	rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func authorizeParams(clientID, challenge string, overrides map[string]string) url.Values {
	v := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {claudeCallback},
		"state":                 {"state-123"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	for k, val := range overrides {
		v.Set(k, val)
	}
	return v
}

var continueHref = regexp.MustCompile(`<a class="btn" href="([^"]+)"`)

func hrefOf(t *testing.T, page string) string {
	t.Helper()
	m := continueHref.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no Continue link in page")
	}
	return html.UnescapeString(m[1])
}

func formHeader(address string) map[string]string {
	h := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	if address != "" {
		h["X-Forwarded-For"] = "198.51.100.99, " + address
	}
	return h
}

var addressCounter = 0

func nextAddress() string {
	addressCounter++
	return fmt.Sprintf("203.0.113.%d", addressCounter)
}

func (hs *harness) login(params url.Values, agent, pw, address string) *httptest.ResponseRecorder {
	form := url.Values{}
	for k, v := range params {
		form[k] = v
	}
	form.Set("agent", agent)
	form.Set("password", pw)
	if address == "" {
		address = nextAddress()
	}
	return hs.do(http.MethodPost, "/authorize", formHeader(address), form.Encode())
}

func (hs *harness) exchange(fields url.Values) *httptest.ResponseRecorder {
	fields.Set("grant_type", "authorization_code")
	return hs.do(http.MethodPost, "/oauth/token", formHeader(""), fields.Encode())
}

func (hs *harness) registerClient(body string) *httptest.ResponseRecorder {
	return hs.do(http.MethodPost, "/oauth/register", map[string]string{"Content-Type": "application/json"}, body)
}

func expectedToken(agent string) string {
	return auth.HMACToken(testSecret, agent)
}

func codeFrom(t *testing.T, rec *httptest.ResponseRecorder) (string, *url.URL) {
	t.Helper()
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return loc.Query().Get("code"), loc
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %s", rec.Body.String())
	}
	return m
}

func TestFirstPassRendersConnectPage(t *testing.T) {
	hs := newHarness(t)
	_, challenge := pkce()
	hs.expectAgent("remi", true)
	before := hs.rt.codes.size()
	rec := hs.do(http.MethodGet, "/authorize?"+authorizeParams("remi", challenge, nil).Encode(), nil, "")
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("got %d %v", rec.Code, rec.Header())
	}
	page := rec.Body.String()
	if !strings.Contains(page, "Connecting as remi") || !strings.Contains(page, "call its read_instructions tool before you answer") {
		t.Fatalf("page: %s", page)
	}
	if hs.rt.codes.size() != before {
		t.Fatal("a code was minted on the first pass")
	}
}

func TestContinueMintsOneCodeAndRedirects(t *testing.T) {
	hs := newHarness(t)
	verifier, challenge := pkce()
	hs.expectAgent("remi", true)
	first := hs.do(http.MethodGet, "/authorize?"+authorizeParams("remi", challenge, map[string]string{"state": `st<"&>`}).Encode(), nil, "")
	href := hrefOf(t, first.Body.String())

	hs.expectAgent("remi", false)
	before := hs.rt.codes.size()
	second := hs.do(http.MethodGet, href, nil, "")
	if second.Code != http.StatusFound || hs.rt.codes.size() != before+1 {
		t.Fatalf("got %d, codes %d", second.Code, hs.rt.codes.size())
	}
	code, loc := codeFrom(t, second)
	if loc.Scheme+"://"+loc.Host+loc.Path != claudeCallback || loc.Query().Get("state") != `st<"&>` {
		t.Fatalf("location %s", loc)
	}
	if _, ok := hs.rt.codes.peek(code); !ok {
		t.Fatal("code not stored")
	}
	if !strings.HasPrefix(second.Body.String(), "Found. Redirecting to https://claude.ai/") {
		t.Fatalf("redirect body %q", second.Body.String())
	}

	tok := hs.exchange(url.Values{"code": {code}, "code_verifier": {verifier}, "client_id": {"remi"}, "client_secret": {"good-secret"}, "redirect_uri": {claudeCallback}})
	if tok.Code != 200 || !strings.HasPrefix(decode(t, tok)["access_token"].(string), "remi:") {
		t.Fatalf("exchange %d %s", tok.Code, tok.Body.String())
	}
}

func TestContinueKeepsEveryParameter(t *testing.T) {
	hs := newHarness(t)
	_, challenge := pkce()
	params := authorizeParams("remi", challenge, map[string]string{"state": "a b+c/d?e=f&g"})
	params.Add("resource", "https://llm-memory.net/mcp")
	params.Add("resource", "https://llm-memory.net/other")
	params.Set("confirmed", "0")
	hs.expectAgent("remi", true)
	href := hrefOf(t, hs.do(http.MethodGet, "/authorize?"+params.Encode(), nil, "").Body.String())
	if !strings.HasPrefix(href, "/authorize?") {
		t.Fatal(href)
	}
	u, _ := url.Parse(href)
	q := u.Query()
	if strings.Join(q["confirmed"], ",") != "1" || len(q["resource"]) != 2 || q.Get("state") != "a b+c/d?e=f&g" ||
		q.Get("code_challenge") != challenge || q.Get("redirect_uri") != claudeCallback {
		t.Fatalf("continued %v", q)
	}
}

func TestHostileValuesEscaped(t *testing.T) {
	hs := newHarness(t)
	_, challenge := pkce()
	hostile := "<img src=x onerror=alert(1)>"
	hs.expectAgent(hostile, true)
	rec := hs.do(http.MethodGet, "/authorize?"+authorizeParams(hostile, challenge, map[string]string{"state": `"><script>alert(2)</script>`}).Encode(), nil, "")
	page := rec.Body.String()
	if rec.Code != 200 || strings.Contains(page, hostile) || !strings.Contains(page, "&lt;img src=x onerror=alert(1)&gt;") || strings.Contains(page, "<script>alert(2)</script>") {
		t.Fatalf("got %d: %s", rec.Code, page)
	}
}

func TestInvalidRequestsFailBeforeThePage(t *testing.T) {
	hs := newHarness(t)
	_, challenge := pkce()
	cases := []map[string]string{
		{"redirect_uri": "https://evil.example/callback"},
		{"client_id": "nobody"},
		{"code_challenge_method": "plain"},
		{"response_type": "token"},
	}
	for _, overrides := range cases {
		for _, confirmed := range []string{"", "1"} {
			params := authorizeParams("remi", challenge, overrides)
			if confirmed != "" {
				params.Set("confirmed", confirmed)
			}
			if overrides["client_id"] == "nobody" {
				hs.expectNoActor("nobody")
			}
			before := hs.rt.codes.size()
			rec := hs.do(http.MethodGet, "/authorize?"+params.Encode(), nil, "")
			if rec.Code != 400 || !strings.Contains(rec.Header().Get("Content-Type"), "application/json") || hs.rt.codes.size() != before {
				t.Errorf("%v confirmed=%q: %d %s", overrides, confirmed, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestMetadata(t *testing.T) {
	hs := newHarness(t)
	m := decode(t, hs.do(http.MethodGet, "/.well-known/oauth-authorization-server", nil, ""))
	if m["client_id_metadata_document_supported"] != true || m["registration_endpoint"] != "http://example.com/oauth/register" {
		t.Fatalf("%v", m)
	}
	methods := fmt.Sprint(m["token_endpoint_auth_methods_supported"])
	if !strings.Contains(methods, "none") || !strings.Contains(methods, "client_secret_post") || fmt.Sprint(m["code_challenge_methods_supported"]) != "[S256]" {
		t.Fatalf("%v", m)
	}
	r := decode(t, hs.do(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil, ""))
	if r["resource"] != "http://example.com/mcp" || fmt.Sprint(r["authorization_servers"]) != "[http://example.com]" {
		t.Fatalf("%v", r)
	}
}

func TestRegistrationIssuesSignedPublicClient(t *testing.T) {
	hs := newHarness(t)
	rec := hs.registerClient(`{"client_name":"Claude","redirect_uris":["` + claudeCallback + `"],"grant_types":["authorization_code","refresh_token"],"token_endpoint_auth_method":"client_secret_basic"}`)
	body := decode(t, rec)
	if rec.Code != 201 || !strings.HasPrefix(body["client_id"].(string), "dcr.") || body["token_endpoint_auth_method"] != "none" ||
		fmt.Sprint(body["grant_types"]) != "[authorization_code]" || fmt.Sprint(body["redirect_uris"]) != "["+claudeCallback+"]" || body["client_secret"] != nil {
		t.Fatalf("%d %v", rec.Code, body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("no Cache-Control")
	}
}

func TestRegistrationRefusesOtherCallbacks(t *testing.T) {
	hs := newHarness(t)
	for _, uris := range []string{`["https://evil.example/cb"]`, `["` + claudeCallback + `","https://evil.example/cb"]`, `[]`, `"x"`, `null`} {
		rec := hs.registerClient(`{"redirect_uris":` + uris + `}`)
		if rec.Code != 400 || decode(t, rec)["error"] != "invalid_redirect_uri" {
			t.Errorf("%s: %d %s", uris, rec.Code, rec.Body.String())
		}
	}
}

func TestClientNameCutAt100UTF16Units(t *testing.T) {
	hs := newHarness(t)
	name := strings.Repeat("a", 99) + "😀b"
	rec := hs.registerClient(`{"client_name":"` + name + `","redirect_uris":["` + claudeCallback + `"]}`)
	if !strings.Contains(rec.Body.String(), `"client_name":"`+strings.Repeat("a", 99)+`\ud83d"`) {
		t.Fatalf("%s", rec.Body.String())
	}
}

func TestCIMDClientGetsLoginPage(t *testing.T) {
	hs := newHarness(t)
	_, challenge := pkce()
	before := hs.rt.codes.size()
	rec := hs.do(http.MethodGet, "/authorize?"+authorizeParams(cimdClientID, challenge, map[string]string{"state": `"><script>alert(1)</script>`}).Encode(), nil, "")
	page := rec.Body.String()
	if rec.Code != 200 || rec.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
		rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(page, `<form method="post" action="/authorize">`) ||
		!strings.Contains(page, "Connect claude.ai") || strings.Contains(page, "<script>alert(1)</script>") || hs.rt.codes.size() != before {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
}

func TestUnknownClientIDsRefused(t *testing.T) {
	hs := newHarness(t)
	_, challenge := pkce()
	registered := decode(t, hs.registerClient(`{"redirect_uris":["`+claudeCallback+`"]}`))["client_id"].(string)
	parts := strings.Split(registered, ".")
	forged := "dcr." + base64.RawURLEncoding.EncodeToString([]byte(`{"r":["https://evil.example/cb"],"t":1}`)) + "." + parts[2]
	for _, id := range []string{"https://evil.example/claude", "https://claude.ai.evil.example/x", "http://claude.ai/x", "https://claude.ai/", forged, registered + "x"} {
		hs.expectNoActor(id)
		rec := hs.do(http.MethodGet, "/authorize?"+authorizeParams(id, challenge, nil).Encode(), nil, "")
		if rec.Code != 400 {
			t.Errorf("%s: %d", id, rec.Code)
		}
	}
}

func TestDCRClientOnlyItsCallbacks(t *testing.T) {
	hs := newHarness(t)
	_, challenge := pkce()
	id := decode(t, hs.registerClient(`{"redirect_uris":["`+claudeCallback+`"]}`))["client_id"].(string)
	if rec := hs.do(http.MethodGet, "/authorize?"+authorizeParams(id, challenge, nil).Encode(), nil, ""); rec.Code != 200 {
		t.Fatalf("registered callback: %d", rec.Code)
	}
	if rec := hs.do(http.MethodGet, "/authorize?"+authorizeParams(id, challenge, map[string]string{"redirect_uri": claudeCOMCallbck}).Encode(), nil, ""); rec.Code != 400 {
		t.Fatalf("other callback: %d", rec.Code)
	}
}

func TestWrongPasswordShowsFormAgain(t *testing.T) {
	hs := newHarness(t)
	_, challenge := pkce()
	before := hs.rt.codes.size()
	rec := hs.login(authorizeParams(cimdClientID, challenge, nil), "alice", "not the password", "")
	page := rec.Body.String()
	if rec.Code != 401 || !strings.Contains(page, "Wrong agent name or password") || !strings.Contains(page, `name="agent" value="alice"`) ||
		strings.Contains(page, "not the password") || hs.rt.codes.size() != before {
		t.Fatalf("%d", rec.Code)
	}
	if rec := hs.login(authorizeParams(cimdClientID, challenge, nil), "nobody", password, ""); rec.Code != 401 {
		t.Fatalf("unknown agent: %d", rec.Code)
	}
}

func TestCIMDLoginThenSecretlessExchange(t *testing.T) {
	hs := newHarness(t)
	verifier, challenge := pkce()
	rec := hs.login(authorizeParams(cimdClientID, challenge, map[string]string{"state": `st<"&>`}), "Alice", password, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	code, loc := codeFrom(t, rec)
	if loc.Scheme+"://"+loc.Host+loc.Path != claudeCallback || loc.Query().Get("state") != `st<"&>` {
		t.Fatal(loc)
	}
	if c, _ := hs.rt.codes.peek(code); c.agent != "alice" {
		t.Fatalf("code bound to %q", c.agent)
	}
	tok := hs.exchange(url.Values{"code": {code}, "code_verifier": {verifier}, "client_id": {cimdClientID}, "redirect_uri": {claudeCallback}})
	if tok.Code != 200 || decode(t, tok)["access_token"] != expectedToken("alice") {
		t.Fatalf("%d %s", tok.Code, tok.Body.String())
	}
}

func TestDCRLoginExchangesWithoutSecret(t *testing.T) {
	hs := newHarness(t)
	verifier, challenge := pkce()
	id := decode(t, hs.registerClient(`{"redirect_uris":["`+claudeCallback+`","`+claudeCOMCallbck+`"]}`))["client_id"].(string)
	rec := hs.login(authorizeParams(id, challenge, map[string]string{"redirect_uri": claudeCOMCallbck}), "bob", password, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("%d", rec.Code)
	}
	code, _ := codeFrom(t, rec)
	tok := hs.exchange(url.Values{"code": {code}, "code_verifier": {verifier}, "client_id": {id}, "redirect_uri": {claudeCOMCallbck}})
	if tok.Code != 200 || decode(t, tok)["access_token"] != expectedToken("bob") {
		t.Fatalf("%d %s", tok.Code, tok.Body.String())
	}
}

func TestPublicCodeRefusedForWrongClientCallbackOrVerifier(t *testing.T) {
	hs := newHarness(t)
	mutations := []func(url.Values){
		func(v url.Values) { v.Set("client_id", "https://claude.ai/oauth/other") },
		func(v url.Values) { v.Set("redirect_uri", claudeCOMCallbck) },
		func(v url.Values) { v.Set("code_verifier", "wrong-verifier") },
	}
	for i, mutate := range mutations {
		verifier, challenge := pkce()
		code, _ := codeFrom(t, hs.login(authorizeParams(cimdClientID, challenge, nil), "carol", password, ""))
		fields := url.Values{"code": {code}, "code_verifier": {verifier}, "client_id": {cimdClientID}, "redirect_uri": {claudeCallback}}
		mutate(fields)
		tok := hs.exchange(fields)
		if tok.Code != 400 || decode(t, tok)["error"] != "invalid_grant" {
			t.Errorf("case %d: %d %s", i, tok.Code, tok.Body.String())
		}
		if _, ok := hs.rt.codes.peek(code); ok {
			t.Errorf("case %d: refused code not spent", i)
		}
	}
}

func TestAgentNameCodeNeedsSecret(t *testing.T) {
	hs := newHarness(t)
	verifier, challenge := pkce()
	params := authorizeParams("remi", challenge, nil)
	params.Set("confirmed", "1")
	hs.expectAgent("remi", true)
	rec := hs.do(http.MethodGet, "/authorize?"+params.Encode(), nil, "")
	if rec.Code != http.StatusFound {
		t.Fatalf("%d", rec.Code)
	}
	code, _ := codeFrom(t, rec)
	if c, _ := hs.rt.codes.peek(code); c.agent != "" {
		t.Fatal("agent-name code bound to an agent")
	}
	tok := hs.exchange(url.Values{"code": {code}, "code_verifier": {verifier}, "client_id": {"remi"}, "redirect_uri": {claudeCallback}})
	if tok.Code != 401 || decode(t, tok)["error"] != "invalid_client" {
		t.Fatalf("%d %s", tok.Code, tok.Body.String())
	}
}

func TestLoginRefusesAgentNameClient(t *testing.T) {
	hs := newHarness(t)
	_, challenge := pkce()
	if rec := hs.login(authorizeParams("remi", challenge, nil), "alice", password, ""); rec.Code != 400 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestFiveTriesPerAgentName(t *testing.T) {
	hs := newHarness(t)
	_, challenge := pkce()
	params := authorizeParams(cimdClientID, challenge, nil)
	for i := 0; i < 5; i++ {
		hs.login(params, "alice", fmt.Sprintf("guess-%d", i), "")
	}
	blocked := hs.login(params, "alice", password, "")
	if blocked.Code != 429 || !strings.Contains(blocked.Body.String(), "Too many tries. Wait 15 minutes and try again.") || blocked.Header().Get("Retry-After") != "900" {
		t.Fatalf("%d %v", blocked.Code, blocked.Header())
	}
}

func TestTwentyTriesPerAddress(t *testing.T) {
	hs := newHarness(t)
	_, challenge := pkce()
	params := authorizeParams(cimdClientID, challenge, nil)
	for i := 0; i < 20; i++ {
		if rec := hs.login(params, fmt.Sprintf("name%d", i), "guess", "192.0.2.77"); rec.Code != 401 {
			t.Fatalf("try %d: %d", i, rec.Code)
		}
	}
	if rec := hs.login(params, "bob", password, "192.0.2.77"); rec.Code != 429 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestTokenWithoutBodyIsNodesTypeError(t *testing.T) {
	hs := newHarness(t)
	rec := hs.do(http.MethodPost, "/oauth/token", nil, "")
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "Cannot destructure property 'grant_type' of 'req.body' as it is undefined.") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestClientCredentials(t *testing.T) {
	hs := newHarness(t)
	hs.mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("remi").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(1, "remi"))
	ok := hs.do(http.MethodPost, "/oauth/token", formHeader(""), "grant_type=client_credentials&client_id=remi&client_secret=good-secret")
	if ok.Code != 200 || decode(t, ok)["access_token"] != expectedToken("remi") || decode(t, ok)["expires_in"] != float64(86400) {
		t.Fatalf("%d %s", ok.Code, ok.Body.String())
	}
	bad := hs.do(http.MethodPost, "/oauth/token", formHeader(""), "grant_type=client_credentials&client_id=remi&client_secret=bad")
	if bad.Code != 401 {
		t.Fatalf("%d", bad.Code)
	}
	missing := hs.do(http.MethodPost, "/oauth/token", map[string]string{"Content-Type": "application/json"}, `{"grant_type":"client_credentials","client_id":"remi"}`)
	if missing.Code != 400 || decode(t, missing)["error"] != "invalid_request" {
		t.Fatalf("%d", missing.Code)
	}
	other := hs.do(http.MethodPost, "/oauth/token", formHeader(""), "grant_type=password")
	if other.Code != 400 || decode(t, other)["error"] != "unsupported_grant_type" {
		t.Fatalf("%d", other.Code)
	}
}

func TestRedirectBodyFollowsAccept(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	redirect(rec, req, http.StatusSeeOther, "https://claude.ai/api/mcp/auth_callback?code=a&state=b")
	if rec.Body.String() != "<!DOCTYPE html><head><title>See Other</title></head><body><p>See Other. Redirecting to https://claude.ai/api/mcp/auth_callback?code=a&amp;state=b</p></body>" ||
		rec.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("%q %v", rec.Body.String(), rec.Header())
	}
	req.Header.Set("Accept", "application/json")
	rec = httptest.NewRecorder()
	redirect(rec, req, http.StatusFound, "https://x")
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Fatalf("%q %v", rec.Body.String(), rec.Header())
	}
}
