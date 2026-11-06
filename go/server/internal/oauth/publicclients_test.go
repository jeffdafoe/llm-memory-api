package oauth

import (
	"encoding/base64"
	"testing"
	"time"
)

// Ported from node/api/src/services/oauth-public-clients.test.js.

func testClients() *PublicClients {
	return &PublicClients{SigningSecret: func() (string, error) { return testSecret, nil }}
}

func TestCIMDClientIDs(t *testing.T) {
	for _, id := range []string{cimdClientID, "https://claude.com/x"} {
		if !IsCIMDClientID(id) {
			t.Errorf("%q refused", id)
		}
	}
	for _, id := range []string{
		"https://claude.ai/", "https://claude.ai", "http://claude.ai/x", "https://claude.ai:8443/x",
		"https://evil@claude.ai/x", "https://claude.ai/x#frag", "https://claude.ai.evil.example/x",
		"https://evil.example/claude.ai/x", "remi", "",
	} {
		if IsCIMDClientID(id) {
			t.Errorf("%q accepted", id)
		}
	}
}

func TestDCRRoundTrip(t *testing.T) {
	p := testClients()
	id, err := p.IssueDCRClientID([]string{claudeCallback}, 1760000000)
	if err != nil {
		t.Fatal(err)
	}
	uris, ok, err := p.VerifyDCRClientID(id)
	if err != nil || !ok || len(uris) != 1 || uris[0] != claudeCallback {
		t.Fatalf("%v %v %v", uris, ok, err)
	}
	if kind, _ := p.Kind(id); kind != "dcr" {
		t.Fatalf("kind %q", kind)
	}
}

func TestTamperedDCRRefused(t *testing.T) {
	p := testClients()
	id, _ := p.IssueDCRClientID([]string{claudeCallback}, 1760000000)
	parts := splitDot(id)
	sig := parts[2]
	first := "A"
	if sig[0] == 'A' {
		first = "B"
	}
	for _, bad := range []string{
		parts[0] + "." + parts[1] + "." + first + sig[1:],
		parts[0] + "." + parts[1] + "x." + sig,
		parts[0] + "." + parts[1],
		"xyz." + parts[1] + "." + sig,
	} {
		if _, ok, _ := p.VerifyDCRClientID(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func splitDot(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// signRaw signs any payload, standing in for a future issuer bug or a leaked
// key; the verifier must still refuse what IssueDCRClientID never writes.
func signRaw(p *PublicClients, payloadJSON string) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(payloadJSON))
	sig, _ := p.sign(payload)
	return dcrPrefix + payload + "." + sig
}

func TestSignedPayloadMustMatchFormat(t *testing.T) {
	p := testClients()
	for _, payload := range []string{
		`{"r":["https://evil.example/cb"],"t":1}`,
		`{"r":[],"t":1}`,
		`{"r":["` + claudeCallback + `","` + claudeCallback + `"],"t":1}`,
		`{"r":["` + AllowedRedirectURIs[0] + `","` + AllowedRedirectURIs[1] + `","https://claude.ai/other"],"t":1}`,
		`{"r":["` + claudeCallback + `"],"t":-1}`,
		`{"r":["` + claudeCallback + `"],"t":1.5}`,
		`{"r":["` + claudeCallback + `"],"t":"1"}`,
		`{"r":["` + claudeCallback + `"]}`,
		`{"r":["` + claudeCallback + `"],"t":null}`,
		`{"r":[42],"t":1}`,
		`[1]`,
		`null`,
	} {
		if _, ok, _ := p.VerifyDCRClientID(signRaw(p, payload)); ok {
			t.Errorf("%s accepted", payload)
		}
	}
	if _, ok, _ := p.VerifyDCRClientID(signRaw(p, `{"r":["`+claudeCallback+`"],"t":1e3}`)); !ok {
		t.Error("1e3 is a safe integer in Node")
	}
}

func TestDCRWithoutSecretIsAnError(t *testing.T) {
	p := &PublicClients{SigningSecret: func() (string, error) { return "", nil }}
	if _, err := p.IssueDCRClientID([]string{claudeCallback}, 1); err != ErrNoSigningSecret {
		t.Fatalf("%v", err)
	}
	if _, err := p.Kind("dcr.a.b"); err != ErrNoSigningSecret {
		t.Fatalf("%v", err)
	}
	if kind, err := p.Kind("remi"); kind != "" || err != nil {
		t.Fatalf("%q %v", kind, err)
	}
}

func TestCodesExpireAndAreSpent(t *testing.T) {
	now := time.Unix(1000, 0)
	s := newCodeStore(func() time.Time { return now })
	code := s.mint(authCode{clientID: "remi"})
	if _, ok := s.take(code); !ok {
		t.Fatal("fresh code refused")
	}
	if _, ok := s.take(code); ok {
		t.Fatal("code used twice")
	}
	code = s.mint(authCode{clientID: "remi"})
	now = now.Add(60*time.Second + time.Millisecond)
	if _, ok := s.take(code); ok {
		t.Fatal("expired code accepted")
	}
	s.mint(authCode{})
	now = now.Add(61 * time.Second)
	s.mint(authCode{})
	if s.size() != 1 {
		t.Fatalf("sweep left %d codes", s.size())
	}
}
