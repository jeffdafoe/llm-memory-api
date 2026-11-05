package httpapi

import (
	"net/http"
	"strings"
)

// RawPath is the request path as the client sent it, before percent-decoding
// and without the query. Express's req.path is this raw form, so path checks
// copied from the Node code compare against it, not r.URL.Path:
// "/v1/agent%2Flogin" is not the login route in Node.
func RawPath(r *http.Request) string {
	if strings.HasPrefix(r.RequestURI, "/") {
		p, _, _ := strings.Cut(r.RequestURI, "?")
		return p
	}
	return r.URL.EscapedPath()
}
