package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRawMux(t *testing.T) {
	text := func(s string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(s)) })
	}
	m := NewRawMux("/v1", text("fallback"))
	m.Handle("POST", "/memory/search", text("search"))
	m.Handle("GET", "/things", text("things"))
	m.Handle("DELETE", "/things", text("deleted"))

	cases := []struct{ method, target, body, allow string }{
		{"POST", "/v1/memory/search", "search", ""},
		{"POST", "/v1/memory/search?x=1", "search", ""},
		{"GET", "/v1/memory/search", "fallback", ""},
		{"POST", "/v1/memory/%73earch", "fallback", ""},
		{"POST", "/v1/memory/search/", "fallback", ""},
		{"POST", "/v1//memory/search", "fallback", ""},
		{"POST", "/v1/Memory/search", "fallback", ""},
		{"OPTIONS", "/v1/memory/search", "POST", "POST"},
		{"HEAD", "/v1/things", "", ""},
		{"OPTIONS", "/v1/things", "DELETE, GET, HEAD", "DELETE, GET, HEAD"},
		{"OPTIONS", "/v1/nothing", "fallback", ""},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, httptest.NewRequest(c.method, c.target, nil))
		if c.method != "HEAD" && rec.Body.String() != c.body {
			t.Errorf("%s %s: body %q, want %q", c.method, c.target, rec.Body.String(), c.body)
		}
		if got := rec.Header().Get("Allow"); got != c.allow {
			t.Errorf("%s %s: Allow %q, want %q", c.method, c.target, got, c.allow)
		}
	}
}

func TestFields(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	if _, ok := BodyFields(WithStateValue(r, &State{})); ok {
		t.Error("no JSON body must read as an undefined req.body")
	}
	arr, ok := BodyFields(WithStateValue(r, &State{Body: json.RawMessage(`[1]`)}))
	if !ok || len(arr) != 0 {
		t.Errorf("an array body has no fields: %v %v", arr, ok)
	}

	body := `{"s":"x","e":"","z":0,"nz":-0.0,"n":2,"big":1e400,"tiny":1e-400,"t":true,"f":false,"nul":null,"a":[],"o":{},"dup":1,"dup":0}`
	f, ok := BodyFields(WithStateValue(r, &State{Body: json.RawMessage(body)}))
	if !ok {
		t.Fatal("object body")
	}
	truthy := map[string]bool{"s": true, "e": false, "z": false, "nz": false, "n": true, "big": true, "tiny": false,
		"t": true, "f": false, "nul": false, "a": true, "o": true, "missing": false, "dup": false}
	for name, want := range truthy {
		if got := f.Truthy(name); got != want {
			t.Errorf("Truthy(%s) = %v, want %v", name, got, want)
		}
	}
	if s, ok := f.String("s"); !ok || s != "x" {
		t.Errorf("String(s) = %q %v", s, ok)
	}
	if _, ok := f.String("n"); ok {
		t.Error("a number is not a string")
	}
	if a, ok := f.Array("a"); !ok || a == nil && len(a) != 0 {
		t.Error("Array(a)")
	}
	if _, ok := f.Array("o"); ok {
		t.Error("an object is not an array")
	}
	if !strings.Contains(string(f.Raw("o")), "{") || f.Raw("missing") != nil {
		t.Error("Raw")
	}
}
