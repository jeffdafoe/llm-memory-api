// Package privacy serves GET /privacy, the operator's privacy policy
// (LLM-1021). The whole policy is one config value, privacy_policy, written
// in markdown by whoever runs this server: it names their controller, their
// hosting and their AI providers, so none of it can live in the repo. The
// page reads the value on every request, so an edit in the admin config
// shows at once.
//
// A policy can name a retention period with a placeholder such as
// {{conversation_retention_days}}; the page fills it from the same config
// key the cleanup job reads, so the stated period follows the configured one
// (see effectiveDays for off and invalid values). Only keys ending in
// _retention_days are filled. The config table also
// holds secrets (mcp_oauth_bearer_secret, virtual_agent_encryption_key), and
// a free {{any_key}} would let a typo in the policy publish one on a public
// page; any other placeholder is left as written.
package privacy

import (
	"bytes"
	"html/template"
	"net/http"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
)

// PolicyKey is the config key that holds the policy markdown.
const PolicyKey = "privacy_policy"

// Path is where the page is served.
const Path = "/privacy"

// Logger is the part of *logger.Logger the page uses.
type Logger interface {
	Errorf(format string, args ...any)
}

// Routes serves the page.
type Routes struct {
	Config *config.Store
	Log    Logger
}

// Register adds GET /privacy (and HEAD, which the mux answers with GET).
func (rt *Routes) Register(m *httpapi.RawMux) {
	m.Handle(http.MethodGet, Path, http.HandlerFunc(rt.page))
}

// placeholder matches {{name}}, with optional spaces inside the braces.
var placeholder = regexp.MustCompile(`\{\{\s*([a-z0-9_]+)\s*\}\}`)

// allowedPlaceholder reports whether a config key may be shown on the page.
func allowedPlaceholder(key string) bool {
	return strings.HasSuffix(key, "_retention_days")
}

// scriptKeys are read by the shell scripts (db-cleanup.sh, pg-backup.sh),
// which use 7 for a missing key or any value that is not a whole number from
// 1 to 999999. The page shows what the scripts will use.
var scriptKeys = map[string]bool{
	"request_log_retention_days": true,
	"backup_retention_days":      true,
}

// wholeDays is a whole number of days the page will state.
var wholeDays = regexp.MustCompile(`^[0-9]{1,6}$`)

// effectiveDays is the period the page states for a retention key: the
// configured whole number of days when it is 1 or more. A script key that is
// missing or invalid states 7, as the scripts do. Any other key that is
// missing, 0 (off) or invalid states nothing (ok false): the page cannot
// honestly give a number, so the placeholder shows as written and the
// operator rewords that line.
func effectiveDays(key string, store *config.Store) (days string, ok bool) {
	raw, found := store.Lookup(key)
	text := strings.TrimSpace(raw)
	if found && wholeDays.MatchString(text) {
		if trimmed := strings.TrimLeft(text, "0"); trimmed != "" {
			return trimmed, true
		}
	}
	if scriptKeys[key] {
		return "7", true
	}
	return "", false
}

// fillPlaceholders replaces each allowed placeholder with the period it
// stands for. A placeholder that is not allowed, or has no period to state,
// stays exactly as written, so the operator sees it on the page and fixes it.
func fillPlaceholders(text string, store *config.Store) string {
	return placeholder.ReplaceAllStringFunc(text, func(match string) string {
		key := placeholder.FindStringSubmatch(match)[1]
		if !allowedPlaceholder(key) {
			return match
		}
		days, ok := effectiveDays(key, store)
		if !ok {
			return match
		}
		return days
	})
}

// markdown renders CommonMark plus GitHub's tables, strikethrough and
// autolinks. goldmark's default renderer leaves raw HTML out (it writes an
// HTML comment in its place) and drops links with dangerous schemes such as
// javascript:, so the policy text cannot inject script into the page. Do not
// add html.WithUnsafe().
var markdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

// Render turns the policy markdown into the page's HTML body. An empty
// policy gives "" and ok false.
func Render(policy string, store *config.Store) (body template.HTML, ok bool, err error) {
	if strings.TrimSpace(policy) == "" {
		return "", false, nil
	}
	var out bytes.Buffer
	if err := markdown.Convert([]byte(fillPlaceholders(policy, store)), &out); err != nil {
		return "", false, err
	}
	// goldmark escaped the text and left raw HTML out, so its output is safe
	// to place in the page as is.
	return template.HTML(out.String()), true, nil
}

// pageTemplate is the whole page. html/template escapes everything that is
// not template.HTML, so only the rendered markdown goes in unescaped.
var pageTemplate = template.Must(template.New("privacy").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Privacy Policy</title>
<style>
:root { color-scheme: light dark; --text: #1d1d1f; --muted: #5f6368; --line: #d9d9de; --bg: #ffffff; --link: #0b57d0; }
@media (prefers-color-scheme: dark) { :root { --text: #e8e8ea; --muted: #a3a3ab; --line: #3a3a40; --bg: #141416; --link: #8ab4f8; } }
body { margin: 0; background: var(--bg); color: var(--text); font: 16px/1.6 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; }
main { max-width: 46rem; margin: 0 auto; padding: 2rem 1rem 4rem; }
h1, h2, h3 { line-height: 1.25; }
h2 { margin-top: 2rem; }
a { color: var(--link); }
table { border-collapse: collapse; width: 100%; margin: 1rem 0; display: block; overflow-x: auto; }
th, td { border: 1px solid var(--line); padding: .45rem .6rem; text-align: left; vertical-align: top; }
code { font-size: .9em; }
.missing { color: var(--muted); }
</style>
</head>
<body>
<main>
{{if .HasPolicy}}{{.Body}}{{else}}<h1>Privacy Policy</h1>
<p class="missing">The operator of this service has not published a privacy policy yet.</p>{{end}}
</main>
</body>
</html>
`))

// page writes the policy, or the not-published page when privacy_policy is
// empty or missing (a server whose migration has not run yet). Both are 200,
// so links to /privacy never break.
func (rt *Routes) page(w http.ResponseWriter, r *http.Request) {
	policy, _ := rt.Config.Lookup(PolicyKey)
	body, ok, err := Render(policy, rt.Config)
	if err != nil {
		rt.Log.Errorf("privacy: render failed: %s", err)
		httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "The privacy policy could not be shown")
		return
	}
	var out bytes.Buffer
	if err := pageTemplate.Execute(&out, struct {
		HasPolicy bool
		Body      template.HTML
	}{ok, body}); err != nil {
		rt.Log.Errorf("privacy: page failed: %s", err)
		httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "The privacy policy could not be shown")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	// The page needs nothing but its own inline style: no scripts, frames,
	// forms or remote fonts. Images only from this server (a logo under
	// /static), so opening the policy never tells another host about the
	// visitor.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	// The policy can change in the admin config at any time.
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(out.Bytes())
	}
}
