package apibase

import (
	"strings"
	"testing"
)

func TestBase(t *testing.T) {
	t.Setenv("OPENAI_API_BASE", "")
	if got, _ := Base("OPENAI_API_BASE"); got != "https://api.openai.com" {
		t.Errorf("default = %q", got)
	}
	t.Setenv("OPENAI_API_BASE", "  http://127.0.0.1:9100/node/openai//  ")
	if got, err := Base("OPENAI_API_BASE"); err != nil || got != "http://127.0.0.1:9100/node/openai" {
		t.Errorf("with a path prefix = %q, %v", got, err)
	}
	bad := map[string]string{
		"ftp://x":                     "must be an http or https URL",
		"http://x/?a=1":               "must not have a query",
		"http://x/#frag":              "must not have a query",
		"http://user:secret@x":        "must not have a query",
		"not a url with secret-value": "is not a valid URL",
	}
	for value, want := range bad {
		t.Setenv("OPENAI_API_BASE", value)
		_, err := Base("OPENAI_API_BASE")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", value, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("the error quotes the value: %v", err)
		}
		if Check() == nil {
			t.Errorf("Check passed with %q", value)
		}
	}
	if _, err := Base("NOPE"); err == nil {
		t.Error("unknown variable")
	}
}
