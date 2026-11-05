package oauth

import (
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
)

// The Express and WHATWG behavior these routes relied on: URLSearchParams
// for the URLs they build, and res.redirect's content negotiation.

type searchPair struct{ name, value string }

// parseSearch is the application/x-www-form-urlencoded parser of the URL
// standard (URLSearchParams): '&'-separated, empty sequences skipped, split
// at the first '=', '+' is a space, %XX escapes decoded as bytes, and the
// bytes read as UTF-8 with the WHATWG decoder's replacement.
func parseSearch(s string) []searchPair {
	var out []searchPair
	for _, seq := range strings.Split(s, "&") {
		if seq == "" {
			continue
		}
		name, value, _ := strings.Cut(seq, "=")
		out = append(out, searchPair{formDecode(name), formDecode(value)})
	}
	return out
}

func formDecode(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	return httpapi.DecodeUTF8([]byte(percentDecode(s)))
}

// setSearch is URLSearchParams.set: the first pair with the name takes the
// value and later ones are removed; with none, the pair is appended.
func setSearch(pairs []searchPair, name, value string) []searchPair {
	out := pairs[:0:0]
	found := false
	for _, p := range pairs {
		if p.name != name {
			out = append(out, p)
			continue
		}
		if !found {
			out = append(out, searchPair{name, value})
			found = true
		}
	}
	if !found {
		out = append(out, searchPair{name, value})
	}
	return out
}

// serializeSearch is the urlencoded serializer: space is '+', alphanumerics
// and *-._ stay, every other byte of the UTF-8 is %XX.
func serializeSearch(pairs []searchPair) string {
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		formEncode(&b, p.name)
		b.WriteByte('=')
		formEncode(&b, p.value)
	}
	return b.String()
}

func formEncode(b *strings.Builder, s string) {
	const hexDigits = "0123456789ABCDEF"
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			b.WriteByte('+')
		case c == '*' || c == '-' || c == '.' || c == '_' ||
			c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&15])
		}
	}
}

// redirect is Express's res.redirect(status, address) for an address that
// needs no further encoding: Location, then a body chosen by the Accept
// header (text/plain first, text/html, or empty), and Vary: Accept.
func redirect(w http.ResponseWriter, r *http.Request, status int, address string) {
	h := w.Header()
	h.Set("Location", address)
	h.Add("Vary", "Accept")
	var body string
	switch negotiateTextHTML(r) {
	case "text":
		h.Set("Content-Type", "text/plain; charset=utf-8")
		body = http.StatusText(status) + ". Redirecting to " + address
	case "html":
		h.Set("Content-Type", "text/html; charset=utf-8")
		body = "<!DOCTYPE html><head><title>" + http.StatusText(status) + "</title></head>" +
			"<body><p>" + http.StatusText(status) + ". Redirecting to " + escapeHTML(address) + "</p></body>"
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		w.Write([]byte(body))
	}
}

// negotiateTextHTML is req.accepts(['text', 'html']) (accepts + negotiator):
// "text", "html", or "" when the Accept header allows neither. A missing or
// empty header takes the first.
func negotiateTextHTML(r *http.Request) string {
	accept := strings.Join(r.Header.Values("Accept"), ", ")
	if accept == "" {
		return "text"
	}
	specs := parseAccept(accept)
	provided := []struct{ name, typ, sub string }{{"text", "text", "plain"}, {"html", "text", "html"}}
	type priority struct {
		o, i int
		q    float64
		s    int
		ok   bool
	}
	best := ""
	var bestP priority
	for index, p := range provided {
		pr := priority{o: -1, q: 0, s: 0}
		for _, spec := range specs {
			s := 0
			if strings.EqualFold(spec.typ, p.typ) {
				s |= 4
			} else if spec.typ != "*" {
				continue
			}
			if strings.EqualFold(spec.sub, p.sub) {
				s |= 2
			} else if spec.sub != "*" {
				continue
			}
			if len(spec.params) > 0 {
				all := true
				for _, v := range spec.params {
					// The provided types have no parameters.
					if v != "*" && v != "" {
						all = false
					}
				}
				if !all {
					continue
				}
				s |= 1
			}
			cand := priority{o: spec.i, q: spec.q, s: s, i: index, ok: true}
			if jsOr(float64(pr.s-cand.s), pr.q-cand.q, float64(pr.o-cand.o)) < 0 {
				pr = cand
			}
		}
		pr.i = index
		if !(pr.q > 0) {
			continue
		}
		if best == "" || compareSpecs(pr.q, pr.s, pr.o, pr.i, bestP.q, bestP.s, bestP.o, bestP.i) < 0 {
			best, bestP = p.name, pr
		}
	}
	return best
}

// compareSpecs is negotiator's sort order; negative puts a first.
func compareSpecs(aq float64, as, ao, ai int, bq float64, bs, bo, bi int) float64 {
	return jsOr(bq-aq, float64(bs-as), float64(ao-bo), float64(ai-bi))
}

// jsOr is JavaScript's a || b || c over numbers: the first truthy one (not
// 0, not NaN), else the last.
func jsOr(values ...float64) float64 {
	for _, v := range values {
		if v != 0 && !math.IsNaN(v) {
			return v
		}
	}
	return values[len(values)-1]
}

type acceptSpec struct {
	typ, sub string
	params   map[string]string
	q        float64
	i        int
}

// parseAccept is negotiator's parseAccept: media ranges split on commas
// outside quotes; a range that is not type/subtype is dropped; q ends the
// parameters.
func parseAccept(accept string) []acceptSpec {
	var out []acceptSpec
	for i, part := range splitQuoted(accept, ',') {
		spec, ok := parseMediaRange(strings.TrimSpace(part), i)
		if ok {
			out = append(out, spec)
		}
	}
	return out
}

func splitQuoted(s string, sep byte) []string {
	raw := strings.Split(s, string(sep))
	out := []string{raw[0]}
	for _, p := range raw[1:] {
		if strings.Count(out[len(out)-1], `"`)%2 == 0 {
			out = append(out, p)
		} else {
			out[len(out)-1] += string(sep) + p
		}
	}
	return out
}

// parseMediaRange follows /^\s*([^\s\/;]+)\/([^;\s]+)\s*(?:;(.*))?$/.
func parseMediaRange(s string, index int) (acceptSpec, bool) {
	spec := acceptSpec{q: 1, i: index, params: map[string]string{}}
	s = strings.TrimLeft(s, " \t\n\r\f\v")
	slash := strings.IndexByte(s, '/')
	if slash <= 0 || strings.ContainsAny(s[:slash], " \t\n\r\f\v;") {
		return spec, false
	}
	spec.typ = s[:slash]
	rest := s[slash+1:]
	end := 0
	for end < len(rest) && rest[end] != ';' && !isJSSpaceByte(rest[end]) {
		end++
	}
	if end == 0 {
		return spec, false
	}
	spec.sub = rest[:end]
	rest = strings.TrimLeft(rest[end:], " \t\n\r\f\v")
	if rest == "" {
		return spec, true
	}
	if rest[0] != ';' {
		return spec, false
	}
	params := rest[1:]
	if params == "" {
		return spec, true
	}
	for _, kv := range splitQuoted(params, ';') {
		kv = strings.TrimSpace(kv)
		key, val, hasVal := strings.Cut(kv, "=")
		key = strings.ToLower(key)
		if hasVal && len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		if key == "q" {
			spec.q = parseFloatJS(val)
			break
		}
		spec.params[key] = val
	}
	return spec, true
}

func isJSSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// parseFloatJS is parseFloat: the longest decimal prefix after leading
// whitespace, NaN when there is none.
func parseFloatJS(s string) float64 {
	s = strings.TrimLeft(s, " \t\n\r\f\v")
	end := 0
	if end < len(s) && (s[end] == '+' || s[end] == '-') {
		end++
	}
	if strings.HasPrefix(s[end:], "Infinity") {
		if s[0] == '-' {
			return math.Inf(-1)
		}
		return math.Inf(1)
	}
	digits := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
		digits++
	}
	if end < len(s) && s[end] == '.' {
		end++
		for end < len(s) && s[end] >= '0' && s[end] <= '9' {
			end++
			digits++
		}
	}
	if digits == 0 {
		return math.NaN()
	}
	if end < len(s) && (s[end] == 'e' || s[end] == 'E') {
		e := end + 1
		if e < len(s) && (s[e] == '+' || s[e] == '-') {
			e++
		}
		if e < len(s) && s[e] >= '0' && s[e] <= '9' {
			for e < len(s) && s[e] >= '0' && s[e] <= '9' {
				e++
			}
			end = e
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(s[:end], "."), 64)
	if err != nil {
		// Out of range: ParseFloat returns ±Inf or 0 with the error.
		return f
	}
	return f
}

// escapeHTML is escape-html and the pages' escapeHtml: & < > " '.
func escapeHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}
