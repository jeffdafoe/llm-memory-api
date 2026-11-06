package oauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// Public OAuth clients for the claude.ai connector (Node's
// services/oauth-public-clients.js): clients that prove nothing at the token
// endpoint, so the user logs in on /authorize instead. Both kinds use PKCE.
//
//   - CIMD (Client ID Metadata Document): the client_id is an HTTPS URL on one
//     of Anthropic's origins. The document is never fetched: redirect_uri is
//     checked against the fixed allowlist, so it could add nothing.
//   - DCR (RFC 7591): the client_id carries its own registration, signed:
//     "dcr." + base64url(JSON {r: redirect_uris, t: issued_at}) + "." + HMAC.
//     Nothing is stored, and it survives restarts.
//
// Agent-name client_ids never collide with either: agent names are
// [a-z0-9_-] only.

// AllowedRedirectURIs is every redirect_uri an authorization_code client may
// use: the claude.ai callback and its claude.com successor.
var AllowedRedirectURIs = []string{
	"https://claude.ai/api/mcp/auth_callback",
	"https://claude.com/api/mcp/auth_callback",
}

var cimdHosts = []string{"claude.ai", "claude.com"}

const (
	dcrPrefix = "dcr."
	// dcrSigningLabel keeps a DCR signature from ever matching an HMAC made
	// with the same secret for something else (the per-agent bearer token).
	dcrSigningLabel = "llm-memory oauth dcr client_id v1\n"
)

// ErrNoSigningSecret is Node's error when mcp_oauth_bearer_secret is empty.
var ErrNoSigningSecret = errors.New("mcp_oauth_bearer_secret is not configured")

func allowedRedirect(uri string) bool {
	_, ok := allowedRedirectTarget(uri)
	return ok
}

// allowedRedirectTarget returns the allowlist entry equal to uri.
func allowedRedirectTarget(uri string) (string, bool) {
	for _, a := range AllowedRedirectURIs {
		if uri == a {
			return a, true
		}
	}
	return "", false
}

// IsCIMDClientID reports whether clientID is a URL on an Anthropic origin
// with a path, as Node checked it through the WHATWG URL parser (cimdurl.go).
func IsCIMDClientID(clientID string) bool {
	if jsutil.UTF16Len(clientID) > 512 {
		return false
	}
	return cimdURL(clientID)
}

// PublicClients signs and checks DCR client_ids with the secret from
// SigningSecret (config mcp_oauth_bearer_secret).
type PublicClients struct {
	SigningSecret func() (string, error)
}

func (p *PublicClients) secret() (string, error) {
	s, err := p.SigningSecret()
	if err != nil {
		return "", err
	}
	if s == "" {
		return "", ErrNoSigningSecret
	}
	return s, nil
}

func (p *PublicClients) sign(payload string) (string, error) {
	secret, err := p.secret()
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(dcrSigningLabel + payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// IssueDCRClientID signs a registration. redirectURIs must already be checked
// against the allowlist and deduplicated.
func (p *PublicClients) IssueDCRClientID(redirectURIs []string, issuedAt int64) (string, error) {
	var b strings.Builder
	b.WriteString(`{"r":[`)
	for i, u := range redirectURIs {
		if i > 0 {
			b.WriteByte(',')
		}
		quoted, _ := json.Marshal(u)
		b.Write(quoted)
	}
	b.WriteString(`],"t":`)
	b.WriteString(jsutil.NumberString(float64(issuedAt)))
	b.WriteByte('}')
	payload := base64.RawURLEncoding.EncodeToString([]byte(b.String()))
	sig, err := p.sign(payload)
	if err != nil {
		return "", err
	}
	return dcrPrefix + payload + "." + sig, nil
}

// VerifyDCRClientID returns the registered redirect URIs of a client_id this
// server signed, or ok false. err is set only when the signing secret is not
// configured, which Node threw.
func (p *PublicClients) VerifyDCRClientID(clientID string) (redirectURIs []string, ok bool, err error) {
	if !strings.HasPrefix(clientID, dcrPrefix) || jsutil.UTF16Len(clientID) > 2048 {
		return nil, false, nil
	}
	parts := strings.Split(clientID[len(dcrPrefix):], ".")
	if len(parts) != 2 {
		return nil, false, nil
	}
	payload, signature := parts[0], parts[1]
	expected, err := p.sign(payload)
	if err != nil {
		return nil, false, err
	}
	if len(signature) != len(expected) || subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) != 1 {
		return nil, false, nil
	}
	// Hold the payload to exactly what IssueDCRClientID writes, not only to
	// the signature: a leaked key must not widen what a client may do.
	var decoded map[string]json.RawMessage
	if json.Unmarshal(nodeBase64Decode(payload), &decoded) != nil || decoded == nil {
		return nil, false, nil
	}
	// Decoded as any, so null (which unmarshals into a float64 as 0) and
	// a missing t are refused.
	var t any
	if json.Unmarshal(decoded["t"], &t) != nil {
		return nil, false, nil
	}
	issuedAt, isNumber := t.(float64)
	if !isNumber || !isSafeInteger(issuedAt) || issuedAt < 0 {
		return nil, false, nil
	}
	var uris []json.RawMessage
	if json.Unmarshal(decoded["r"], &uris) != nil || len(uris) == 0 || len(uris) > len(AllowedRedirectURIs) {
		return nil, false, nil
	}
	seen := map[string]bool{}
	for _, raw := range uris {
		var u string
		if json.Unmarshal(raw, &u) != nil || !allowedRedirect(u) || seen[u] {
			return nil, false, nil
		}
		seen[u] = true
		redirectURIs = append(redirectURIs, u)
	}
	return redirectURIs, true, nil
}

func isSafeInteger(x float64) bool {
	return x == math.Trunc(x) && math.Abs(x) <= 1<<53-1
}

// nodeBase64Decode decodes as Buffer.from(s, 'base64url') does: both
// alphabets, and any other character is skipped. Only a payload this server
// signed reaches it.
func nodeBase64Decode(s string) []byte {
	var clean strings.Builder
	for _, c := range s {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			clean.WriteRune(c)
		case c == '+' || c == '-':
			clean.WriteByte('-')
		case c == '/' || c == '_':
			clean.WriteByte('_')
		case c == '=':
			// Padding ends the data.
			return decodeRaw(clean.String())
		}
	}
	return decodeRaw(clean.String())
}

func decodeRaw(s string) []byte {
	// A single leftover character carries no full byte.
	if len(s)%4 == 1 {
		s = s[:len(s)-1]
	}
	out, _ := base64.RawURLEncoding.DecodeString(s)
	return out
}

// Kind returns "cimd" or "dcr" for a public client, "" for anything else (an
// agent name, or garbage).
func (p *PublicClients) Kind(clientID string) (string, error) {
	if IsCIMDClientID(clientID) {
		return "cimd", nil
	}
	_, ok, err := p.VerifyDCRClientID(clientID)
	if err != nil {
		return "", err
	}
	if ok {
		return "dcr", nil
	}
	return "", nil
}
