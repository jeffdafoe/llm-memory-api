package oauth

import (
	"strconv"
	"strings"
)

// Loopback redirect URIs for native clients such as Claude Code (RFC 8252
// section 7.3). The client listens on a port of this computer, so a code sent
// there never leaves the machine whose browser logged in, and PKCE makes it
// useless to any other local process that catches it.

// claudeCodeCIMDClientID is Claude Code's own CIMD client_id. It sends this
// one, not a DCR registration, when the server advertises CIMD. Its document
// lists http://localhost/callback and http://127.0.0.1/callback; the document
// is not fetched, so those two are fixed here.
const claudeCodeCIMDClientID = "https://claude.ai/oauth/claude-code-client-metadata"

var claudeCodeCallbacks = []loopbackURI{
	{host: "localhost", path: "/callback"},
	{host: "127.0.0.1", path: "/callback"},
}

// maxDCRRedirectURIs caps one registration. It keeps the signed client_id
// well under VerifyDCRClientID's 2048-character limit.
const maxDCRRedirectURIs = 5

// maxLoopbackLength bounds one loopback URI.
const maxLoopbackLength = 200

var loopbackHosts = []string{"localhost", "127.0.0.1", "[::1]"}

type loopbackURI struct {
	host string
	port string
	path string
}

// String is the URI rebuilt from its checked parts.
func (l loopbackURI) String() string {
	return "http://" + l.host + ":" + l.port + l.path
}

// sameHostAndPath is the RFC 8252 match: any port.
func (l loopbackURI) sameHostAndPath(other loopbackURI) bool {
	return l.host == other.host && l.path == other.path
}

// parseLoopback accepts only http://HOST:PORT/PATH, where HOST is localhost,
// 127.0.0.1 or [::1] exactly, PORT is 1-65535 without leading zeros, and
// PATH holds only unreserved characters and slashes. So userinfo, a query, a
// fragment, percent-encoding and look-alike hosts all fail, and String gives
// back the same text.
func parseLoopback(uri string) (loopbackURI, bool) {
	if len(uri) > maxLoopbackLength {
		return loopbackURI{}, false
	}
	rest, found := strings.CutPrefix(uri, "http://")
	if !found {
		return loopbackURI{}, false
	}
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return loopbackURI{}, false
	}
	authority := rest[:slash]
	path := rest[slash:]
	colon := strings.LastIndexByte(authority, ':')
	if colon < 0 {
		return loopbackURI{}, false
	}
	host := authority[:colon]
	port := authority[colon+1:]
	knownHost := false
	for _, h := range loopbackHosts {
		if host == h {
			knownHost = true
		}
	}
	if !knownHost || !validPort(port) || !validLoopbackPath(path) {
		return loopbackURI{}, false
	}
	return loopbackURI{host: host, port: port, path: path}, true
}

func validPort(port string) bool {
	if port == "" || len(port) > 5 || port[0] == '0' {
		return false
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 1 && n <= 65535
}

func validLoopbackPath(path string) bool {
	if !strings.HasPrefix(path, "/") {
		return false
	}
	for _, c := range path {
		isAlnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if !isAlnum && !strings.ContainsRune("-._~/", c) {
			return false
		}
	}
	return true
}

// registrableRedirect reports whether a DCR client may register uri: an
// allowlisted claude.ai callback, or a loopback URI.
func registrableRedirect(uri string) bool {
	if allowedRedirect(uri) {
		return true
	}
	_, ok := parseLoopback(uri)
	return ok
}

// redirectMatches reports whether a request's redirect_uri fits a registered
// one: the same string, or for loopback the same host and path on any port.
func redirectMatches(registered, requested string) bool {
	if registered == requested {
		return true
	}
	r, ok1 := parseLoopback(registered)
	q, ok2 := parseLoopback(requested)
	return ok1 && ok2 && r.sameHostAndPath(q)
}

// isClaudeCodeCallback reports whether the CIMD client clientID may send its
// code to the loopback URI l: only Claude Code's client_id, and only to the
// callbacks its document lists.
func isClaudeCodeCallback(clientID string, l loopbackURI) bool {
	if clientID != claudeCodeCIMDClientID {
		return false
	}
	for _, c := range claudeCodeCallbacks {
		if c.sameHostAndPath(l) {
			return true
		}
	}
	return false
}

// redirectTarget is where a code may be sent: the allowlist's own string, or
// a loopback URI rebuilt from its parsed parts. Never the request's raw text.
func redirectTarget(uri string) (string, bool) {
	if target, ok := allowedRedirectTarget(uri); ok {
		return target, true
	}
	if l, ok := parseLoopback(uri); ok {
		return l.String(), true
	}
	return "", false
}
