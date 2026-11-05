// Package apibase gives the base URL of each outside API the server calls
// (Node's services/api-base.js). Production sets none of the variables and
// gets the public origins. The contract tests set them so every provider call
// and the embeddings call go to a local fake server. A base may carry a path
// prefix; the caller appends its own path.
package apibase

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Defaults are the production origins, by variable name.
var Defaults = map[string]string{
	"OPENAI_API_BASE":     "https://api.openai.com",
	"ANTHROPIC_API_BASE":  "https://api.anthropic.com",
	"GOOGLE_API_BASE":     "https://generativelanguage.googleapis.com",
	"OPENROUTER_API_BASE": "https://openrouter.ai",
	"PERPLEXITY_API_BASE": "https://api.perplexity.ai",
	"XAI_API_BASE":        "https://api.x.ai",
}

// Base returns the base for name, read from the environment on every call.
// The errors never quote the value: a mistyped one can hold a key or a
// password, and the message goes to the journal.
func Base(name string) (string, error) {
	def, ok := Defaults[name]
	if !ok {
		return "", fmt.Errorf("Unknown API base variable %s", name)
	}
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("%s is not a valid URL", name)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%s must be an http or https URL", name)
	}
	// The caller appends its path, so a query or fragment would land in the
	// middle of the URL.
	if strings.ContainsAny(raw, "?#") || u.User != nil {
		return "", fmt.Errorf("%s must not have a query, fragment or credentials", name)
	}
	return strings.TrimRight(raw, "/"), nil
}

// order is api-base.js's key order, so the first bad variable reported is
// the same one Node reports.
var order = []string{
	"OPENAI_API_BASE", "ANTHROPIC_API_BASE", "GOOGLE_API_BASE",
	"OPENROUTER_API_BASE", "PERPLEXITY_API_BASE", "XAI_API_BASE",
}

// Check validates every variable, so a bad value stops the server at start
// instead of failing each call later.
func Check() error {
	for _, n := range order {
		if _, err := Base(n); err != nil {
			return err
		}
	}
	return nil
}
