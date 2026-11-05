package httpapi

import (
	"net/http"
	"path"
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

// isClean reports whether ServeMux would serve p as is. ServeMux redirects
// any other path to its cleaned form ("//a/./b" to "/a/b"), keeping a
// trailing slash; this is the same cleaning rule.
func isClean(p string) bool {
	if p == "" || p[0] != '/' {
		return false
	}
	np := path.Clean(p)
	if p[len(p)-1] == '/' && np != "/" {
		np += "/"
	}
	return np == p
}

// Unclean sends a request whose path ServeMux would redirect to fallback,
// and every other request to next. Express never cleans or redirects a path:
// "//a" or "/v1/../x" simply matches no route. fallback gives the answer an
// unmatched path gets at that level.
func Unclean(next, fallback http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isClean(r.URL.Path) {
			fallback.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
