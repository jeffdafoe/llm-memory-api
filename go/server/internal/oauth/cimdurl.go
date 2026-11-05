package oauth

import (
	"strconv"
	"strings"
)

// cimdURL answers Node's isCimdClientId test after the length check: does
// `new URL(clientID)` parse, with an origin of https://claude.ai or
// https://claude.com, a pathname longer than "/", and no username, password
// or fragment? It follows the WHATWG URL parser (Ada, in Node) only as far as
// that answer needs: the scheme, the special-scheme authority, the host, the
// port, the path's dot segments, and a blob: URL's origin (that of the URL in
// its path).
//
// One difference is kept on purpose: a host that reaches claude.ai or
// claude.com only through IDNA mapping (non-ASCII characters such as
// full-width letters) is not recognised; the port would need a UTS 46
// implementation for it (go-server note, Known differences).
func cimdURL(input string) bool {
	scheme, rest, ok := splitScheme(preprocess(input))
	if !ok {
		return false
	}
	switch scheme {
	case "https":
		u, ok := parseHTTPS(rest)
		return ok && u.anthropicOrigin() && u.username == "" && u.password == "" &&
			u.fragment == "" && !(len(u.path) == 1 && u.path[0] == "")
	case "blob":
		return blobCIMD(rest)
	}
	return false
}

// preprocess strips leading and trailing C0 controls and spaces and removes
// every tab and newline, as the URL parser does before anything else.
func preprocess(s string) string {
	s = strings.TrimFunc(s, func(r rune) bool { return r <= 0x20 })
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, s)
}

// splitScheme returns the lowercased scheme and what follows its colon. With
// no base URL, input without a scheme fails to parse.
func splitScheme(s string) (scheme, rest string, ok bool) {
	if s == "" || !isASCIIAlpha(s[0]) {
		return "", "", false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if c == ':' {
			return strings.ToLower(s[:i]), s[i+1:], true
		}
		if !isASCIIAlpha(c) && !(c >= '0' && c <= '9') && c != '+' && c != '-' && c != '.' {
			return "", "", false
		}
	}
	return "", "", false
}

func isASCIIAlpha(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

type specialURL struct {
	username, password string
	host               string // lowercased; "" when not a plain ASCII domain
	port               string // "" for none or the default 443
	path               []string
	fragment           string
}

func (u specialURL) anthropicOrigin() bool {
	if u.port != "" {
		return false
	}
	for _, h := range cimdHosts {
		if u.host == h {
			return true
		}
	}
	return false
}

// parseHTTPS parses what follows "https:". ok is false where the URL parser
// fails.
func parseHTTPS(rest string) (specialURL, bool) {
	var u specialURL
	// Special authority (ignore) slashes: any run of / or \, even none.
	i := 0
	for i < len(rest) && (rest[i] == '/' || rest[i] == '\\') {
		i++
	}
	rest = rest[i:]

	end := strings.IndexAny(rest, "/\\?#")
	if end < 0 {
		end = len(rest)
	}
	authority, remainder := rest[:end], rest[end:]

	hostPort := authority
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		userinfo := authority[:at]
		hostPort = authority[at+1:]
		if hostPort == "" {
			return u, false
		}
		u.username, u.password, _ = strings.Cut(userinfo, ":")
	}

	host, port, hasPort := splitHostPort(hostPort)
	if host == "" {
		return u, false
	}
	if hasPort && port != "" {
		for j := 0; j < len(port); j++ {
			if port[j] < '0' || port[j] > '9' {
				return u, false
			}
		}
		trimmed := strings.TrimLeft(port, "0")
		if len(trimmed) > 5 {
			return u, false
		}
		n := 0
		if trimmed != "" {
			n, _ = strconv.Atoi(trimmed)
		}
		if n > 65535 {
			return u, false
		}
		if n != 443 {
			u.port = strconv.Itoa(n)
		}
	}
	h, ok := parseHost(host)
	if !ok {
		return u, false
	}
	u.host = h

	pathPart := remainder
	if q := strings.IndexAny(pathPart, "?#"); q >= 0 {
		pathPart = remainder[:q]
	}
	if f := strings.IndexByte(remainder, '#'); f >= 0 {
		u.fragment = remainder[f+1:]
	}
	if pathPart != "" {
		// The path start state consumes the leading / or \.
		pathPart = pathPart[1:]
	}
	u.path = specialPath(pathPart)
	return u, true
}

// splitHostPort splits at the first colon outside brackets.
func splitHostPort(s string) (host, port string, hasPort bool) {
	inBrackets := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			inBrackets = true
		case ']':
			inBrackets = false
		case ':':
			if !inBrackets {
				return s[:i], s[i+1:], true
			}
		}
	}
	return s, "", false
}

// parseHost returns the lowercased ASCII domain, "" (with ok) for a host
// that parses but can never be an Anthropic one (an IP address, or a
// non-ASCII name), and ok false where the parser fails.
func parseHost(h string) (string, bool) {
	if strings.HasPrefix(h, "[") {
		if !strings.HasSuffix(h, "]") {
			return "", false
		}
		return "", true
	}
	decoded := percentDecode(h)
	for i := 0; i < len(decoded); i++ {
		if decoded[i] >= 0x80 {
			return "", true
		}
	}
	lower := strings.ToLower(decoded)
	for i := 0; i < len(lower); i++ {
		if forbiddenDomainByte(lower[i]) {
			return "", false
		}
	}
	return lower, true
}

// forbiddenDomainByte is the URL standard's forbidden domain code point set,
// for ASCII.
func forbiddenDomainByte(c byte) bool {
	if c <= 0x20 || c == 0x7F {
		return true
	}
	return strings.IndexByte("#%/:<>?@[\\]^|", c) >= 0
}

// percentDecode decodes %XX triplets and leaves any other % as it is.
func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

func isSingleDot(s string) bool {
	return s == "." || strings.EqualFold(s, "%2e")
}

func isDoubleDot(s string) bool {
	switch strings.ToLower(s) {
	case "..", ".%2e", "%2e.", "%2e%2e":
		return true
	}
	return false
}

// specialPath is the path state of a special URL over p (the path after its
// leading slash): segments split on / or \, with "." and ".." resolved.
func specialPath(p string) []string {
	segs := []string{}
	start := 0
	for i := 0; i <= len(p); i++ {
		atEnd := i == len(p)
		if !atEnd && p[i] != '/' && p[i] != '\\' {
			continue
		}
		buf := p[start:i]
		switch {
		case isDoubleDot(buf):
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
			if atEnd {
				segs = append(segs, "")
			}
		case isSingleDot(buf):
			if atEnd {
				segs = append(segs, "")
			}
		default:
			segs = append(segs, buf)
		}
		start = i + 1
	}
	return segs
}

// blobCIMD handles "blob:<rest>". A blob URL's origin is that of the http(s)
// URL in its path, its pathname is that whole opaque path, and it has no
// credentials.
func blobCIMD(rest string) bool {
	if strings.HasPrefix(rest, "/") {
		// Not an opaque path; its pathname never parses as a URL.
		return false
	}
	opaque := rest
	if q := strings.IndexAny(rest, "?#"); q >= 0 {
		opaque = rest[:q]
	}
	if f := strings.IndexByte(rest, '#'); f >= 0 && rest[f+1:] != "" {
		return false
	}
	pathname := encodeC0(opaque)
	if len(pathname) <= 1 {
		return false
	}
	scheme, innerRest, ok := splitScheme(preprocess(pathname))
	if !ok || scheme != "https" {
		return false
	}
	inner, ok := parseHTTPS(innerRest)
	return ok && inner.anthropicOrigin()
}

// encodeC0 percent-encodes with the C0 control percent-encode set: controls,
// DEL and every non-ASCII byte.
func encodeC0(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c >= 0x7F {
			b.WriteString("%" + strings.ToUpper(strconv.FormatInt(int64(c)>>4, 16)) + strings.ToUpper(strconv.FormatInt(int64(c)&15, 16)))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
