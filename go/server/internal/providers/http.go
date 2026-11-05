package providers

import (
	"context"
	"fmt"
	"io"
	"net/http"
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

// send makes one request. Headers are combined as fetch's Headers combines
// them: names compared ignoring case, a repeated name's values joined with
// ", ", and each value trimmed of surrounding HTTP whitespace.
func (p *Providers) send(ctx context.Context, method, url string, headers []header, body *string) (*response, error) {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(*body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	var names []string
	values := map[string]string{}
	for _, h := range headers {
		key := strings.ToLower(h.name)
		v := strings.Trim(h.value, " \t\r\n")
		if prev, ok := values[key]; ok {
			values[key] = prev + ", " + v
			continue
		}
		names = append(names, h.name)
		values[key] = v
	}
	for _, n := range names {
		req.Header.Set(n, values[strings.ToLower(n)])
	}
	resp, err := p.client().Do(req)
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
