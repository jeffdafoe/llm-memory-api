package privacy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/config"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
)

type nopLog struct{}

func (nopLog) Errorf(string, ...any) {}

func storeOf(values map[string]string) *config.Store {
	store := config.NewStore()
	for k, v := range values {
		store.Set(k, v)
	}
	return store
}

// get serves one request through a mux built the way the server builds the
// top level.
func get(t *testing.T, store *config.Store, method string) *httptest.ResponseRecorder {
	t.Helper()
	top := httpapi.NewRawMux("", http.HandlerFunc(httpapi.NotFound))
	(&Routes{Config: store, Log: nopLog{}}).Register(top)
	w := httptest.NewRecorder()
	top.ServeHTTP(w, httptest.NewRequest(method, Path, nil))
	return w
}

const notPublished = "has not published a privacy policy yet"

// An empty, blank or missing policy shows the not-published page, never a
// page with blanks, and never a 404 that would break the links to it.
func TestEmptyPolicyFailsClosed(t *testing.T) {
	for name, store := range map[string]*config.Store{
		"missing key": storeOf(nil),
		"empty":       storeOf(map[string]string{PolicyKey: ""}),
		"blank":       storeOf(map[string]string{PolicyKey: " \n\t "}),
	} {
		w := get(t, store, http.MethodGet)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), notPublished) {
			t.Errorf("%s: %d %q", name, w.Code, w.Body.String())
		}
	}
}

func TestPolicyRenders(t *testing.T) {
	policy := "# Our Policy\n\nContact **ops@example.org**.\n\n| Data | Kept |\n|---|---|\n| Logs | 7 days |\n"
	w := get(t, storeOf(map[string]string{PolicyKey: policy}), http.MethodGet)
	body := w.Body.String()
	// GFM autolinks the contact address, so it comes out as a mailto link.
	for _, want := range []string{"<h1>Our Policy</h1>", `<strong><a href="mailto:ops@example.org">ops@example.org</a></strong>`, "<table>", "<td>7 days</td>"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, notPublished) {
		t.Error("not-published text shown with a policy set")
	}
}

// Raw HTML and dangerous link schemes in the policy never reach the page.
func TestPolicyCannotInjectScript(t *testing.T) {
	policy := "# P\n\n<script>alert(1)</script>\n\n<img src=x onerror=alert(2)>\n\n[click](javascript:alert(3))\n\nText <b onclick=x>inline</b>.\n"
	body := get(t, storeOf(map[string]string{PolicyKey: policy}), http.MethodGet).Body.String()
	for _, bad := range []string{"<script>alert", "onerror=", "javascript:", "onclick="} {
		if strings.Contains(body, bad) {
			t.Errorf("page contains %q:\n%s", bad, body)
		}
	}
}

// Retention placeholders are filled from config; anything else, above all a
// secret key, stays exactly as written.
func TestPlaceholderAllowlist(t *testing.T) {
	store := storeOf(map[string]string{
		PolicyKey: "Logs: {{conversation_retention_days}} days. Backups: {{ backup_retention_days }} days. " +
			"Missing: {{nonexistent_retention_days}}. Secret: {{mcp_oauth_bearer_secret}}. Upper: {{Conversation_retention_days}}.",
		"conversation_retention_days": "30",
		"backup_retention_days":       " 7 ",
		"mcp_oauth_bearer_secret":     "s3cr3t-value",
	})
	body := get(t, store, http.MethodGet).Body.String()
	for _, want := range []string{
		"Logs: 30 days.", "Backups: 7 days.",
		"Missing: {{nonexistent_retention_days}}.",
		"Secret: {{mcp_oauth_bearer_secret}}.",
		"Upper: {{Conversation_retention_days}}.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in %s", want, body)
		}
	}
	if strings.Contains(body, "s3cr3t-value") {
		t.Fatal("a secret config value reached the page")
	}
}

// The page states the period the server will actually use. The two script
// keys follow the scripts (invalid or missing: 7). Other keys state only a
// whole number of 1 or more; 0 (off) or a bad value leaves the placeholder.
func TestPlaceholderShowsEffectivePeriod(t *testing.T) {
	cases := []struct {
		key, raw string
		set      bool
		want     string
	}{
		{"request_log_retention_days", "14", true, "14"},
		{"request_log_retention_days", "1 4", true, "7"},
		{"request_log_retention_days", "0", true, "7"},
		{"request_log_retention_days", "7days", true, "7"},
		{"request_log_retention_days", "1234567", true, "7"},
		{"backup_retention_days", "", false, "7"},
		{"backup_retention_days", "007", true, "7"},
		{"error_log_retention_days", " 90 ", true, "90"},
		{"error_log_retention_days", "0", true, "{{error_log_retention_days}}"},
		{"pending_access_request_retention_days", "", true, "{{pending_access_request_retention_days}}"},
		{"conversation_retention_days", "1e3", true, "{{conversation_retention_days}}"},
		{"va_call_log_retention_days", "", false, "{{va_call_log_retention_days}}"},
		// Only ASCII whitespace is trimmed, as read_days does: a value
		// padded with non-breaking spaces is invalid in both places.
		{"request_log_retention_days", " 14 ", true, "7"},
		{"error_log_retention_days", " 90", true, "{{error_log_retention_days}}"},
		{"backup_retention_days", "\t21\n", true, "21"},
		// The six-digit cap applies only where the reader enforces it.
		{"access_request_retention_days", "1234567", true, "{{access_request_retention_days}}"},
		{"conversation_retention_days", "1234567", true, "1234567"},
	}
	for _, c := range cases {
		values := map[string]string{PolicyKey: "Kept [{{" + c.key + "}}] days."}
		if c.set {
			values[c.key] = c.raw
		}
		body := get(t, storeOf(values), http.MethodGet).Body.String()
		if want := "Kept [" + c.want + "] days."; !strings.Contains(body, want) {
			t.Errorf("%s=%q (set %v): want %q in page", c.key, c.raw, c.set, want)
		}
	}
}

// An edit in the admin config (Store.Set) shows on the next request.
func TestPolicyEditShowsAtOnce(t *testing.T) {
	store := storeOf(map[string]string{PolicyKey: ""})
	if !strings.Contains(get(t, store, http.MethodGet).Body.String(), notPublished) {
		t.Fatal("empty policy not shown as not published")
	}
	store.Set(PolicyKey, "# Now set")
	if body := get(t, store, http.MethodGet).Body.String(); !strings.Contains(body, "<h1>Now set</h1>") {
		t.Fatalf("edit not shown: %s", body)
	}
}

func TestHeaders(t *testing.T) {
	w := get(t, storeOf(map[string]string{PolicyKey: "# P"}), http.MethodGet)
	h := w.Header()
	if h.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("content type %q", h.Get("Content-Type"))
	}
	// No scripts, and images only from this server: a remote image would
	// tell its host about every visitor.
	if csp := h.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") ||
		strings.Contains(csp, "script-src") || !strings.Contains(csp, "img-src 'self' data:;") || strings.Contains(csp, "https:") {
		t.Errorf("csp %q", csp)
	}
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Cache-Control") != "no-cache" {
		t.Errorf("headers %v", h)
	}
}

func TestHeadHasNoBody(t *testing.T) {
	w := get(t, storeOf(map[string]string{PolicyKey: "# P"}), http.MethodHead)
	if w.Code != http.StatusOK || w.Body.Len() != 0 {
		t.Errorf("HEAD: %d, %d bytes", w.Code, w.Body.Len())
	}
}

// The example policy operators copy (docs/privacy-policy-example.md) must
// only use placeholders the server fills, and each must name a key seed.sql
// creates; otherwise an operator's page would show a raw {{...}}. Renaming a
// retention key without updating the example fails here.
func TestExamplePolicyPlaceholdersExist(t *testing.T) {
	example, err := os.ReadFile("../../../../docs/privacy-policy-example.md")
	if err != nil {
		t.Fatal(err)
	}
	seed, err := os.ReadFile("../../../../migrations/seed.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(example)
	start := strings.Index(text, "```markdown\n")
	end := strings.LastIndex(text, "\n```")
	if start < 0 || end <= start {
		t.Fatal("no ```markdown template block in the example")
	}
	template := text[start+len("```markdown\n") : end]

	values := map[string]string{PolicyKey: template}
	keys := placeholder.FindAllStringSubmatch(template, -1)
	if len(keys) == 0 {
		t.Fatal("the template uses no placeholders")
	}
	for _, m := range keys {
		key := m[1]
		if !allowedPlaceholder(key) {
			t.Errorf("{{%s}} is not on the allowlist, so it would show as written", key)
		}
		if !strings.Contains(string(seed), "('"+key+"', ") {
			t.Errorf("{{%s}} names a key seed.sql does not create", key)
		}
		values[key] = "42"
	}
	body := get(t, storeOf(values), http.MethodGet).Body.String()
	if placeholder.MatchString(body) {
		t.Errorf("a placeholder survived rendering: %s", placeholder.FindString(body))
	}
	if !strings.Contains(body, "<table>") {
		t.Error("the template's tables did not render")
	}
}

// Only GET and HEAD are served; a POST goes to the fallback like any other
// unknown route.
func TestOtherMethodsAreNotServed(t *testing.T) {
	w := get(t, storeOf(map[string]string{PolicyKey: "# P"}), http.MethodPost)
	if w.Code == http.StatusOK {
		t.Errorf("POST answered %d", w.Code)
	}
}
