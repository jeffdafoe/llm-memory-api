package providers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apibase"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// header is one request header as a fetch headers object listed it.
type header struct {
	name  string
	value string
}

// response is a provider's answer.
type response struct {
	status int
	body   []byte
}

func (r *response) ok() bool {
	return r.status >= 200 && r.status <= 299
}

// text is response.text(): UTF-8 decoded, a leading byte order mark dropped.
func (r *response) text() string {
	return strings.TrimPrefix(httpapi.DecodeUTF8(r.body), "\uFEFF")
}

// json is response.json().
func (r *response) json() (any, error) {
	return jsutil.Parse([]byte(r.text()))
}

// endpoint is apiBase(name) + path.
func endpoint(name, path string) (string, error) {
	base, err := apibase.Base(name)
	if err != nil {
		return "", err
	}
	return base + path, nil
}

// byteString is WebIDL's ByteString conversion, which fetch applies to header
// names and values: every character must be at most U+00FF and is sent as
// that one byte (é is 0xE9, not UTF-8).
func byteString(s string) (string, error) {
	var b strings.Builder
	index := 0
	for _, r := range s {
		if r > 0xFF {
			unit := r
			if r >= 0x10000 {
				unit = 0xD800 + ((r - 0x10000) >> 10)
			}
			return "", fmt.Errorf("Cannot convert argument to a ByteString because the character at index %d has a value of %d which is greater than 255.", index, unit)
		}
		b.WriteByte(byte(r))
		index++
	}
	return b.String(), nil
}

// isToken reports whether s is an HTTP token (a valid header name).
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// fetchHeaders turns a fetch headers record into request headers as undici
// does: every name and value converted to a ByteString first, then each
// name checked, its value trimmed of HTTP whitespace and checked, and a
// repeated name (ignoring case) joined with ", ". Errors carry undici's text.
func fetchHeaders(headers []header) (http.Header, error) {
	type entry struct{ name, value string }
	converted := make([]entry, len(headers))
	for i, h := range headers {
		name, err := byteString(h.name)
		if err != nil {
			return nil, err
		}
		value, err := byteString(h.value)
		if err != nil {
			return nil, err
		}
		converted[i] = entry{name, value}
	}
	var names []string
	values := map[string]string{}
	for _, e := range converted {
		if !isToken(e.name) {
			return nil, fmt.Errorf("Headers.append: \"%s\" is an invalid header name.", e.name)
		}
		v := strings.Trim(e.value, " \t\r\n")
		if strings.ContainsAny(v, "\x00\r\n") {
			return nil, fmt.Errorf("Headers.append: \"%s\" is an invalid header value.", v)
		}
		key := strings.ToLower(e.name)
		if prev, ok := values[key]; ok {
			values[key] = prev + ", " + v
			continue
		}
		names = append(names, key)
		values[key] = v
	}
	out := http.Header{}
	for _, n := range names {
		out.Set(n, values[n])
	}
	return out, nil
}

// fetchRedirects is fetch's redirect rule on top of the client's own: at
// most 20 redirects, and Authorization dropped when a redirect leaves the
// original origin (scheme, host and port). Go's default would send it on to
// another port or a subdomain.
func fetchRedirects(next func(*http.Request, []*http.Request) error) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 20 {
			return errors.New("redirect count exceeded")
		}
		if origin(req.URL) != origin(via[0].URL) {
			req.Header.Del("Authorization")
		}
		if next != nil {
			return next(req, via)
		}
		return nil
	}
}

// origin is a URL's scheme, host and port, the default port filled in.
func origin(u *neturl.URL) string {
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

// send makes one request with fetch's header and redirect rules.
func (p *Providers) send(ctx context.Context, method, url string, headers []header, body *string) (*response, error) {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(*body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	h, err := fetchHeaders(headers)
	if err != nil {
		return nil, err
	}
	req.Header = h
	client := *p.client()
	client.CheckRedirect = fetchRedirects(client.CheckRedirect)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &response{status: resp.StatusCode, body: data}, nil
}

// postJSON sends body as JSON text to url.
func (p *Providers) postJSON(ctx context.Context, url string, headers []header, body *jsutil.Object) (*response, error) {
	text := jsutil.Marshal(body)
	return p.send(ctx, http.MethodPost, url, headers, &text)
}

// apiError is the error for a non-2xx answer: "<label> API error <status>:
// <body>", logged first as api-error.
func apiError(label string, status int, text string) *APIError {
	return &APIError{Status: status, Message: fmt.Sprintf("%s API error %d: %s", label, status, text)}
}

// headerValue is a header value as fetch reads a JavaScript value: String(v).
func headerValue(v any) (string, error) {
	return jsutil.ValueString(v)
}

// spreadEntries is the own enumerable entries `{...v}` copies: an object's
// members, a string's characters, an array's elements by index; nothing
// for other values.
func spreadEntries(v any) [][2]any {
	var out [][2]any
	switch x := v.(type) {
	case *jsutil.Object:
		for _, k := range x.Keys() {
			out = append(out, [2]any{k, x.Value(k)})
		}
	case []any:
		for i, e := range x {
			out = append(out, [2]any{jsutil.NumberString(float64(i)), e})
		}
	case string:
		units := utf16Units(x)
		for i, u := range units {
			out = append(out, [2]any{jsutil.NumberString(float64(i)), u})
		}
	}
	return out
}

// utf16Units splits s into its UTF-16 code units as strings; half of a
// surrogate pair becomes U+FFFD, the closest Go string can hold.
func utf16Units(s string) []string {
	var out []string
	for _, r := range s {
		if r >= 0x10000 {
			out = append(out, "�", "�")
			continue
		}
		out = append(out, string(r))
	}
	return out
}
