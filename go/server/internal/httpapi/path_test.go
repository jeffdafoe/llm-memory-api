package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRawPath(t *testing.T) {
	cases := map[string]string{
		"/v1/agent%2Flogin?x=1": "/v1/agent%2Flogin",
		"/caf%C3%A9":            "/caf%C3%A9",
		"/plain":                "/plain",
	}
	for target, want := range cases {
		if got := RawPath(httptest.NewRequest("GET", target, nil)); got != want {
			t.Errorf("RawPath(%q) = %q, want %q", target, got, want)
		}
	}
	// A request built in code has no RequestURI; the escaped path stands in.
	r, _ := http.NewRequest("GET", "http://x/a%2Fb", nil)
	if got := RawPath(r); got != "/a%2Fb" {
		t.Errorf("RawPath without RequestURI = %q", got)
	}
}

func TestIsClean(t *testing.T) {
	cases := map[string]bool{
		"/":             true,
		"/health":       true,
		"/v1/":          true,
		"/v1/notes/":    true,
		"//double":      false,
		"/a//b":         false,
		"/a/./b":        false,
		"/v1/../health": false,
		"/a/b/..":       false,
		"":              false,
	}
	for p, want := range cases {
		if got := isClean(p); got != want {
			t.Errorf("isClean(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestUncleanSkipsTheMux(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("mux")) })
	h := Unclean(mux, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("fallback")) }))
	for target, want := range map[string]string{"/a/b": "mux", "//a": "fallback", "/a/../b": "fallback"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", target, nil))
		if rec.Code != 200 || rec.Body.String() != want {
			t.Errorf("%s: %d %q, want %q", target, rec.Code, rec.Body.String(), want)
		}
	}
}
