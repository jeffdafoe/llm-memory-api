package httpapi

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// RawMux routes on the raw request path, below a mount prefix, the way the
// Express routers mounted on that prefix matched static routes: the path as
// sent, not percent-decoded ("/v1/%6Demory/search" is not
// "/v1/memory/search") and never cleaned or redirected. Paths match
// case-sensitively and exactly (go-server note, Known differences).
//
// A path with routes but not for the request's method gets Express's
// automatic OPTIONS answer for OPTIONS, and the fallback otherwise, as the
// request fell through to the next router in Node. A GET route also answers
// HEAD.
type RawMux struct {
	prefix   string
	routes   map[string]map[string]http.Handler
	fallback http.Handler
}

// NewRawMux returns a mux for paths below prefix.
func NewRawMux(prefix string, fallback http.Handler) *RawMux {
	return &RawMux{prefix: prefix, routes: map[string]map[string]http.Handler{}, fallback: fallback}
}

// Handle adds the route method path, path relative to the prefix.
func (m *RawMux) Handle(method, path string, h http.Handler) {
	if m.routes[path] == nil {
		m.routes[path] = map[string]http.Handler{}
	}
	m.routes[path][method] = h
}

func (m *RawMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(RawPath(r), m.prefix)
	methods := m.routes[p]
	if methods == nil {
		m.fallback.ServeHTTP(w, r)
		return
	}
	if h := methods[r.Method]; h != nil {
		h.ServeHTTP(w, r)
		return
	}
	if r.Method == http.MethodHead {
		if h := methods[http.MethodGet]; h != nil {
			h.ServeHTTP(w, r)
			return
		}
	}
	if r.Method == http.MethodOptions {
		allow := map[string]bool{}
		for method := range methods {
			allow[method] = true
			if method == http.MethodGet {
				allow[http.MethodHead] = true
			}
		}
		list := make([]string, 0, len(allow))
		for method := range allow {
			list = append(list, method)
		}
		sort.Strings(list)
		WriteOptions(w, strings.Join(list, ", "))
		return
	}
	m.fallback.ServeHTTP(w, r)
}

// WriteOptions is the automatic OPTIONS response of Express's router: the
// allowed methods, sorted, in the Allow header and as a plain-text body.
func WriteOptions(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	w.Header().Set("Content-Length", strconv.Itoa(len(allow)))
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(allow))
}

// Handles reports whether any route is registered at the path below the
// prefix, for any method.
func (m *RawMux) Handles(path string) bool {
	return m.routes[strings.TrimPrefix(path, m.prefix)] != nil
}
