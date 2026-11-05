package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Params are request parameters as a Node route saw them in req.body or
// req.query: each value is a string, a []string (a repeated key), or, for a
// JSON body, the decoded JSON value (string, json.Number, bool, nil,
// []any, map[string]any).
type Params map[string]any

// String returns the value when it is a string.
func (p Params) String(name string) (string, bool) {
	s, ok := p[name].(string)
	return s, ok
}

// MaxFormBytes is the urlencoded body limit: body-parser's default 100kb.
const MaxFormBytes = 100 * 1024

// maxFormParameters is body-parser's parameterLimit.
const maxFormParameters = 1000

// IsFormType reports whether the Content-Type is
// application/x-www-form-urlencoded.
func IsFormType(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/x-www-form-urlencoded"
}

// FormError is a body the urlencoded parser refused, with the status and
// message Express's error handler answered.
type FormError struct {
	Status  int
	Message string
}

func (e *FormError) Error() string { return e.Message }

// ReadForm is express.urlencoded({ extended: false }) (body-parser 2 with qs
// 6 at depth 0) for a request that has a urlencoded body. ok is false when
// the parser would have skipped the request (no body, or another type); the
// caller then sees what the JSON parser left. A refused body is a
// *FormError.
//
// As with the JSON parser, a Content-Encoding other than identity is refused
// rather than inflated.
func ReadForm(r *http.Request) (params Params, ok bool, err error) {
	if !hasBody(r) || !IsFormType(r.Header.Get("Content-Type")) {
		return nil, false, nil
	}
	_, ctParams, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	charset := strings.ToLower(ctParams["charset"])
	if charset == "" {
		charset = "utf-8"
	}
	if charset != "utf-8" && charset != "iso-8859-1" {
		return nil, true, &FormError{http.StatusUnsupportedMediaType, fmt.Sprintf("unsupported charset \"%s\"", strings.ToUpper(charset))}
	}
	encodingHeader := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if encodingHeader != "" && encodingHeader != "identity" {
		return nil, true, &FormError{http.StatusUnsupportedMediaType, fmt.Sprintf("unsupported content encoding \"%s\"", encodingHeader)}
	}
	if r.ContentLength > MaxFormBytes {
		return nil, true, &FormError{http.StatusRequestEntityTooLarge, "request entity too large"}
	}
	raw, readErr := io.ReadAll(io.LimitReader(r.Body, MaxFormBytes+1))
	if readErr != nil {
		return nil, true, &FormError{http.StatusBadRequest, "request aborted"}
	}
	if len(raw) > MaxFormBytes {
		return nil, true, &FormError{http.StatusRequestEntityTooLarge, "request entity too large"}
	}
	var body string
	if charset == "iso-8859-1" {
		body = latin1(raw)
	} else {
		body = strings.TrimPrefix(string(replaceInvalidUTF8(raw)), "\uFEFF")
	}
	if body == "" {
		return Params{}, true, nil
	}
	if strings.Count(body, "&")+1 > maxFormParameters {
		return nil, true, &FormError{http.StatusRequestEntityTooLarge, "too many parameters"}
	}
	return parseQS(body, charset), true, nil
}

// latin1 maps each byte to the code point of the same value.
func latin1(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		sb.WriteRune(rune(c))
	}
	return sb.String()
}

// parseQS is qs.parse with the options body-parser passes for extended:
// false: depth 0 (a key is never split on brackets), repeated keys combined
// into an array, prototype keys allowed. At depth 0 a key wrapped in one
// pair of brackets ("[name]") still names its content, and the values of
// "name" and "[name]" are merged.
func parseQS(str, charset string) Params {
	str = replaceFold(str, "%5B", "[")
	str = replaceFold(str, "%5D", "]")
	parts := strings.Split(str, "&")
	if len(parts) > maxFormParameters {
		parts = parts[:maxFormParameters]
	}

	// parseValues: key -> value in first-seen order.
	var order []string
	temp := map[string]any{}
	for _, part := range parts {
		pos := strings.Index(part, "]=")
		if pos >= 0 {
			pos++
		} else {
			pos = strings.IndexByte(part, '=')
		}
		var key string
		var val any
		if pos == -1 {
			key = qsDecode(part, charset)
			val = ""
		} else {
			key = qsDecode(part[:pos], charset)
			val = qsDecode(part[pos+1:], charset)
		}
		if existing, ok := temp[key]; ok {
			temp[key] = concatValues(existing, val)
			continue
		}
		order = append(order, key)
		temp[key] = val
	}

	out := Params{}
	for _, key := range jsKeyOrder(order) {
		if key == "" {
			continue
		}
		val := temp[key]
		name := key
		if key == "[]" {
			// An array under index keys "0", "1", ...: never a name a route
			// reads, so it is left out.
			continue
		}
		if len(key) >= 2 && key[0] == '[' && key[len(key)-1] == ']' {
			// "[name]" names its content ("[3]" becomes index key "3").
			name = key[1 : len(key)-1]
		}
		if name == "__proto__" {
			continue
		}
		if existing, ok := out[name]; ok {
			out[name] = mergeValue(existing, val)
		} else {
			out[name] = val
		}
	}
	return out
}

// concatValues is [].concat(a, b) over string / []string values.
func concatValues(a, b any) []string {
	var out []string
	for _, v := range []any{a, b} {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case []string:
			out = append(out, x...)
		}
	}
	return out
}

// mergeValue is qs's utils.merge of a new value into an existing one: an
// empty string adds nothing, anything else is appended.
func mergeValue(existing, val any) any {
	if s, ok := val.(string); ok && s == "" {
		return existing
	}
	return concatValues(existing, val)
}

// jsKeyOrder is the order Object.keys gives: canonical array indexes
// ascending, then the other keys in insertion order.
func jsKeyOrder(keys []string) []string {
	var idx, rest []string
	for _, k := range keys {
		if isArrayIndexText(k) {
			idx = append(idx, k)
		} else {
			rest = append(rest, k)
		}
	}
	for i := 1; i < len(idx); i++ {
		for j := i; j > 0 && indexLess(idx[j], idx[j-1]); j-- {
			idx[j], idx[j-1] = idx[j-1], idx[j]
		}
	}
	return append(idx, rest...)
}

func indexLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// isArrayIndexText reports a canonical array index ("0", "17", not "07"),
// below 2^32 - 1.
func isArrayIndexText(s string) bool {
	if s == "" || len(s) > 10 || (len(s) > 1 && s[0] == '0') {
		return false
	}
	var n uint64
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
		n = n*10 + uint64(s[i]-'0')
	}
	return n < 1<<32-1
}

func replaceFold(s, old, repl string) string {
	if !strings.Contains(strings.ToUpper(s), old) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if i+len(old) <= len(s) && strings.EqualFold(s[i:i+len(old)], old) {
			b.WriteString(repl)
			i += len(old)
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// qsDecode is qs's utils.decode: '+' is a space, then decodeURIComponent,
// keeping the text as it is when that throws. For iso-8859-1 each %XX is
// the code point of that byte.
func qsDecode(s, charset string) string {
	s = strings.ReplaceAll(s, "+", " ")
	if charset == "iso-8859-1" {
		var b strings.Builder
		for i := 0; i < len(s); i++ {
			if s[i] == '%' && i+2 < len(s) && isHexDigit(s[i+1]) && isHexDigit(s[i+2]) {
				b.WriteRune(rune(hexVal(s[i+1])<<4 | hexVal(s[i+2])))
				i += 2
				continue
			}
			b.WriteByte(s[i])
		}
		return b.String()
	}
	if decoded, err := DecodeURIComponent(s); err == nil {
		return decoded
	}
	return s
}

// ErrURI is decodeURIComponent's URIError.
var ErrURI = errors.New("URI malformed")

// DecodeURIComponent decodes %XX escapes as JavaScript's
// decodeURIComponent: every escape must be two hex digits, and the bytes of
// a multi-byte escape must form valid UTF-8, or the whole call fails.
func DecodeURIComponent(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '%' {
			b.WriteByte(s[i])
			i++
			continue
		}
		lead, ok := escapeAt(s, i)
		if !ok {
			return "", ErrURI
		}
		i += 3
		if lead < 0x80 {
			b.WriteByte(lead)
			continue
		}
		n := 0
		switch {
		case lead&0xE0 == 0xC0:
			n = 2
		case lead&0xF0 == 0xE0:
			n = 3
		case lead&0xF8 == 0xF0:
			n = 4
		default:
			return "", ErrURI
		}
		seq := []byte{lead}
		for k := 1; k < n; k++ {
			c, ok := escapeAt(s, i)
			if !ok || c&0xC0 != 0x80 {
				return "", ErrURI
			}
			seq = append(seq, c)
			i += 3
		}
		if !utf8.Valid(seq) {
			return "", ErrURI
		}
		b.Write(seq)
	}
	return b.String(), nil
}

func escapeAt(s string, i int) (byte, bool) {
	if i+2 >= len(s) || s[i] != '%' || !isHexDigit(s[i+1]) || !isHexDigit(s[i+2]) {
		return 0, false
	}
	return hexVal(s[i+1])<<4 | hexVal(s[i+2]), true
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
}

// RawQuery is the query of the request target as Node's HTTP parser gave it
// to Express: each byte as the code point of the same value (latin1), so a
// raw non-ASCII byte is one character.
func RawQuery(r *http.Request) string {
	q := r.URL.RawQuery
	if strings.HasPrefix(r.RequestURI, "/") {
		_, q, _ = strings.Cut(r.RequestURI, "?")
	}
	return latin1([]byte(q))
}

// Query is req.query under Express 5's default "simple" parser,
// node:querystring's parse: pairs split on '&' (at most 1000, empty ones
// counted and skipped), name and value split at the first '=', '+' is a
// space, and a part is decoded only when it holds a valid %XX escape. A
// repeated name becomes a []string.
func Query(r *http.Request) Params {
	return parseQuerystring(RawQuery(r))
}

func parseQuerystring(qs string) Params {
	out := Params{}
	if qs == "" {
		return out
	}
	pairs := 1000
	for _, part := range strings.Split(qs, "&") {
		if part == "" {
			pairs--
			if pairs == 0 {
				return out
			}
			continue
		}
		rawKey, rawVal, hasEq := strings.Cut(part, "=")
		key := querystringPart(rawKey, true)
		val := ""
		if hasEq {
			val = querystringPart(rawVal, false)
		}
		if existing, ok := out[key]; ok {
			out[key] = concatValues(existing, val)
		} else {
			out[key] = val
		}
		pairs--
		if pairs == 0 {
			return out
		}
	}
	return out
}

// querystringPart replaces '+' and decodes one key or value the way
// querystring.parse does. Its scan for a valid escape differs slightly
// between keys and values: in a key a '+' interrupts a half-seen escape.
func querystringPart(s string, isKey bool) string {
	encoded := false
	check := 0
	for i := 0; i < len(s) && !encoded; i++ {
		c := s[i]
		switch {
		case c == '%':
			check = 1
		case check > 0 && isHexDigit(c):
			check++
			if check == 3 {
				encoded = true
			}
		case c == '+' && !isKey:
			// A value's '+' leaves the scan where it was.
		default:
			check = 0
		}
	}
	s = strings.ReplaceAll(s, "+", " ")
	if !encoded || s == "" {
		return s
	}
	// decodeURIComponent keeps every character that is not an escape, so a
	// raw byte stays its latin1 character; the fallback turns it back into
	// that byte.
	if decoded, err := DecodeURIComponent(s); err == nil {
		return decoded
	}
	return unescapeBuffer(s)
}

// unescapeBuffer is querystring.unescapeBuffer(s).toString(): valid %XX
// escapes become bytes, every other character becomes the low byte of its
// code point, and the bytes are read as UTF-8 with replacement.
func unescapeBuffer(s string) string {
	var b []byte
	for i := 0; i < len(s); {
		if s[i] == '%' {
			if c, ok := escapeAt(s, i); ok {
				b = append(b, c)
				i += 3
				continue
			}
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		b = append(b, byte(r))
		i += size
	}
	return string(replaceInvalidUTF8(b))
}

// JSONParams is req.body as express.json left it, decoded: nil when the
// request had no JSON body (undefined in Node), and empty for a top-level
// array, whose named fields are all undefined. Numbers stay json.Number.
func JSONParams(r *http.Request) Params {
	raw := StateOf(r).Body
	if raw == nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return Params{}
	}
	if m, ok := v.(map[string]any); ok {
		return Params(m)
	}
	return Params{}
}
