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
