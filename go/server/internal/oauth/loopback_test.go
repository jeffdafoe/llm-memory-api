package oauth

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestParseLoopbackAccepts(t *testing.T) {
	for _, uri := range []string{
		"http://localhost:53124/callback",
		"http://127.0.0.1:1/callback",
		"http://[::1]:65535/cb/x",
		"http://localhost:8080/",
		"http://localhost:8080/a-b_c.d~e",
	} {
		l, ok := parseLoopback(uri)
		if !ok {
			t.Errorf("%q refused", uri)
			continue
		}
		if l.String() != uri {
			t.Errorf("%q rebuilt as %q", uri, l.String())
		}
	}
}

func TestParseLoopbackRefuses(t *testing.T) {
	for _, uri := range []string{
		"http://localhost.evil.com:80/cb",
		"http://localhost.evil.com/",
		"http://localhost@evil.com/",
		"http://localhost:80@evil.com/",
		"http://evil.com@localhost:80/cb",
		"http://127.0.0.1.nip.io:80/cb",
		"http://127.0.0.1.nip.io/",
		"https://localhost:80/cb",
		"https://localhost/",
		"HTTP://localhost:80/cb",
		"http://LOCALHOST:80/cb",
		"http://localhost/callback",
		"http://localhost/",
		"http://localhost:/callback",
		"http://localhost:0/cb",
		"http://localhost:65536/cb",
		"http://localhost:080/cb",
		"http://localhost:+80/cb",
		"http://localhost:80",
		"http://%6cocalhost:80/cb",
		"http://localhost%2eevil.com:80/cb",
		"http://localhost:80/c%2fb",
		"http://localhost:80/cb?x=1",
		"http://localhost:80/cb#f",
		"http://localhost:80/cb\r\nX: y",
		"http://localhost:80/c b",
		"http://[::1]/cb",
		"http://::1:80/cb",
		"http://[::ffff:127.0.0.1]:80/cb",
		"http://127.0.0.2:80/cb",
		"http://0.0.0.0:80/cb",
		"http://localhost:80/" + strings.Repeat("a", 200),
		"https://claude.ai/api/mcp/auth_callback",
		"",
	} {
		if _, ok := parseLoopback(uri); ok {
			t.Errorf("%q accepted", uri)
		}
	}
}

func TestRedirectMatches(t *testing.T) {
	cases := []struct {
		registered, requested string
		want                  bool
	}{
		{"http://localhost:5000/callback", "http://localhost:6000/callback", true},
		{"http://localhost:5000/callback", "http://localhost:5000/callback", true},
		{"http://localhost:5000/callback", "http://127.0.0.1:5000/callback", false},
		{"http://localhost:5000/callback", "http://localhost:5000/other", false},
		{"http://localhost:5000/callback", "http://localhost:5000/callback/", false},
		{claudeCallback, claudeCallback, true},
		{claudeCallback, claudeCOMCallbck, false},
		{claudeCallback, "http://localhost:5000/callback", false},
	}
	for _, c := range cases {
		if got := redirectMatches(c.registered, c.requested); got != c.want {
			t.Errorf("%q vs %q: got %v", c.registered, c.requested, got)
		}
	}
}

func TestDCRPayloadWithLoopback(t *testing.T) {
	p := testClients()
	uris := []string{claudeCallback, "http://localhost:5000/callback", "http://127.0.0.1:5000/callback", "http://[::1]:5000/callback", claudeCOMCallbck}
	id, err := p.IssueDCRClientID(uris, 1760000000)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := p.VerifyDCRClientID(id)
	if err != nil || !ok || fmt.Sprint(got) != fmt.Sprint(uris) {
		t.Fatalf("%v %v %v", got, ok, err)
	}
	six := `{"r":["http://localhost:1/a","http://localhost:1/b","http://localhost:1/c","http://localhost:1/d","http://localhost:1/e","http://localhost:1/f"],"t":1}`
	if _, ok, _ := p.VerifyDCRClientID(signRaw(p, six)); ok {
		t.Error("six URIs accepted")
	}
	for _, bad := range []string{"http://localhost/callback", "http://localhost.evil.com:80/cb", "http://localhost:80/cb?x=1"} {
		if _, ok, _ := p.VerifyDCRClientID(signRaw(p, `{"r":["`+bad+`"],"t":1}`)); ok {
			t.Errorf("%q accepted in a signed payload", bad)
		}
	}
	long := make([]string, maxDCRRedirectURIs)
	for i := range long {
		long[i] = fmt.Sprintf("http://localhost:%d/%s", 60000+i, strings.Repeat("a", maxLoopbackLength-len("http://localhost:60000/")))
	}
	id, err = p.IssueDCRClientID(long, 1760000000)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := p.VerifyDCRClientID(id); !ok {
		t.Errorf("the largest registration (%d characters) does not verify", len(id))
	}
}

const claudeCodeCallback = "http://localhost:53124/callback"

func TestRegistrationAcceptsLoopback(t *testing.T) {
	hs := newHarness(t)
	rec := hs.registerClient(`{"client_name":"Claude Code (llm-memory)","redirect_uris":["` + claudeCodeCallback + `"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"token_endpoint_auth_method":"none"}`)
	body := decode(t, rec)
	if rec.Code != 201 || !strings.HasPrefix(body["client_id"].(string), "dcr.") || fmt.Sprint(body["redirect_uris"]) != "["+claudeCodeCallback+"]" {
		t.Fatalf("%d %v", rec.Code, body)
	}
	for _, uris := range []string{
		`["http://localhost/callback"]`,
		`["http://localhost.evil.com:80/cb"]`,
		`["` + claudeCodeCallback + `","https://evil.example/cb"]`,
		`["http://localhost:1/a","http://localhost:1/b","http://localhost:1/c","http://localhost:1/d","http://localhost:1/e","http://localhost:1/f"]`,
	} {
		rec := hs.registerClient(`{"redirect_uris":` + uris + `}`)
		if rec.Code != 400 || decode(t, rec)["error"] != "invalid_redirect_uri" {
			t.Errorf("%s: %d %s", uris, rec.Code, rec.Body.String())
		}
	}
}

func (hs *harness) authorizeStatus(clientID, redirectURI string) (int, string) {
	_, challenge := pkce()
	rec := hs.do(http.MethodGet, "/authorize?"+authorizeParams(clientID, challenge, map[string]string{"redirect_uri": redirectURI}).Encode(), nil, "")
	return rec.Code, rec.Body.String()
}

func TestDCRLoopbackAnyPortSameHostAndPath(t *testing.T) {
	hs := newHarness(t)
	id := decode(t, hs.registerClient(`{"redirect_uris":["http://localhost:5000/callback"]}`))["client_id"].(string)
	status, page := hs.authorizeStatus(id, "http://localhost:6000/callback")
	if status != 200 || !strings.Contains(page, "Connect an app on this computer") || strings.Contains(page, "claude.ai does not tell Claude") ||
		strings.Contains(page, "copyButton") || !strings.Contains(page, `<form method="post" action="/authorize">`) {
		t.Fatalf("other port: %d", status)
	}
	for _, uri := range []string{"http://127.0.0.1:6000/callback", "http://localhost:6000/other", claudeCallback} {
		if status, _ := hs.authorizeStatus(id, uri); status != 400 {
			t.Errorf("%s: %d", uri, status)
		}
	}
}

func TestDCRLoopbackLoginAndExchange(t *testing.T) {
	hs := newHarness(t)
	id := decode(t, hs.registerClient(`{"redirect_uris":["http://127.0.0.1:5000/callback"]}`))["client_id"].(string)
	verifier, challenge := pkce()
	requested := "http://127.0.0.1:41234/callback"
	params := authorizeParams(id, challenge, map[string]string{"redirect_uri": requested, "state": `st<"&>`})
	rec := hs.login(params, "alice", password, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	code, loc := codeFrom(t, rec)
	if loc.Scheme+"://"+loc.Host+loc.Path != requested || loc.Query().Get("state") != `st<"&>` {
		t.Fatal(loc)
	}
	tok := hs.exchange(url.Values{"code": {code}, "code_verifier": {verifier}, "client_id": {id}, "redirect_uri": {requested}})
	body := decode(t, tok)
	if _, has := body["expires_in"]; tok.Code != 200 || body["access_token"] != expectedToken("alice") || has {
		t.Fatalf("%d %s", tok.Code, tok.Body.String())
	}

	// The token endpoint holds the exchange to the exact redirect_uri the code
	// was minted for: another port on the same host and path fails.
	verifier, challenge = pkce()
	code, _ = codeFrom(t, hs.login(authorizeParams(id, challenge, map[string]string{"redirect_uri": requested}), "alice", password, ""))
	tok = hs.exchange(url.Values{"code": {code}, "code_verifier": {verifier}, "client_id": {id}, "redirect_uri": {"http://127.0.0.1:5000/callback"}})
	if tok.Code != 400 || decode(t, tok)["error"] != "invalid_grant" {
		t.Fatalf("other port at the token endpoint: %d %s", tok.Code, tok.Body.String())
	}

	// A failed try shows the local page again, not the claude.ai one.
	rec = hs.login(params, "alice", "not the password", "")
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "Connect an app on this computer") {
		t.Fatalf("%d", rec.Code)
	}
}

func TestClaudeCodeCIMDLoopback(t *testing.T) {
	hs := newHarness(t)
	for _, uri := range []string{"http://localhost:53124/callback", "http://127.0.0.1:1/callback", claudeCallback} {
		if status, _ := hs.authorizeStatus(claudeCodeCIMDClientID, uri); status != 200 {
			t.Errorf("%s: %d", uri, status)
		}
	}
	for _, uri := range []string{"http://[::1]:53124/callback", "http://localhost:53124/other", "http://localhost:53124/callback/"} {
		if status, _ := hs.authorizeStatus(claudeCodeCIMDClientID, uri); status != 400 {
			t.Errorf("%s: %d", uri, status)
		}
	}

	verifier, challenge := pkce()
	rec := hs.login(authorizeParams(claudeCodeCIMDClientID, challenge, map[string]string{"redirect_uri": claudeCodeCallback}), "bob", password, "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	code, loc := codeFrom(t, rec)
	if loc.Scheme+"://"+loc.Host+loc.Path != claudeCodeCallback {
		t.Fatal(loc)
	}
	tok := hs.exchange(url.Values{"code": {code}, "code_verifier": {verifier}, "client_id": {claudeCodeCIMDClientID}, "redirect_uri": {claudeCodeCallback}})
	body := decode(t, tok)
	if _, has := body["expires_in"]; tok.Code != 200 || body["access_token"] != expectedToken("bob") || has {
		t.Fatalf("%d %s", tok.Code, tok.Body.String())
	}

	// The same client_id on the claude.ai callback still gets the lifetime.
	verifier, challenge = pkce()
	code, _ = codeFrom(t, hs.login(authorizeParams(claudeCodeCIMDClientID, challenge, nil), "bob", password, ""))
	tok = hs.exchange(url.Values{"code": {code}, "code_verifier": {verifier}, "client_id": {claudeCodeCIMDClientID}, "redirect_uri": {claudeCallback}})
	if tok.Code != 200 || decode(t, tok)["expires_in"] != float64(tokenTTLSeconds) {
		t.Fatalf("%d %s", tok.Code, tok.Body.String())
	}
}

func TestOtherClientsRefuseLoopback(t *testing.T) {
	hs := newHarness(t)
	claudeOnly := decode(t, hs.registerClient(`{"redirect_uris":["`+claudeCallback+`"]}`))["client_id"].(string)
	for _, id := range []string{cimdClientID, "https://claude.ai/oauth/claude-code-client-metadata/", "https://claude.com/oauth/claude-code-client-metadata", "remi", claudeOnly} {
		before := hs.rt.codes.size()
		if status, _ := hs.authorizeStatus(id, claudeCodeCallback); status != 400 {
			t.Errorf("%s: %d", id, status)
		}
		if rec := hs.login(authorizeParams(id, "x", map[string]string{"redirect_uri": claudeCodeCallback}), "carol", password, ""); rec.Code != 400 {
			t.Errorf("%s login: %d", id, rec.Code)
		}
		if hs.rt.codes.size() != before {
			t.Errorf("%s: a code was minted", id)
		}
	}
}

func TestRedirectTargetRebuildsLoopback(t *testing.T) {
	for uri, want := range map[string]string{
		claudeCallback:                    claudeCallback,
		"http://localhost:53124/callback": "http://localhost:53124/callback",
		"http://[::1]:8/cb":               "http://[::1]:8/cb",
	} {
		if got, ok := redirectTarget(uri); !ok || got != want {
			t.Errorf("%q: %q %v", uri, got, ok)
		}
	}
	for _, uri := range []string{"http://localhost@evil.com/", "https://evil.example/cb", "http://localhost/callback"} {
		if _, ok := redirectTarget(uri); ok {
			t.Errorf("%q accepted", uri)
		}
	}
}
