package oauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
)

// goldens.json is written by testdata/goldens.js from Node's own code.
type goldens struct {
	CIMD []struct {
		Input string `json:"input"`
		CIMD  bool   `json:"cimd"`
	} `json:"cimd"`
	DCR []struct {
		URIs     []string `json:"uris"`
		IssuedAt int64    `json:"issuedAt"`
		ClientID string   `json:"clientId"`
		Verified struct {
			RedirectURIs []string `json:"redirectUris"`
		} `json:"verified"`
	} `json:"dcr"`
	Form []struct {
		Input  string         `json:"input"`
		Parsed map[string]any `json:"parsed"`
	} `json:"form"`
	Query []struct {
		Input  string         `json:"input"`
		Parsed map[string]any `json:"parsed"`
	} `json:"query"`
	ContinueURLs []struct {
		Input string `json:"input"`
		URL   string `json:"url"`
	} `json:"continueURLs"`
	Negotiation []struct {
		Accept *string `json:"accept"`
		Chosen string  `json:"chosen"`
	} `json:"negotiation"`
	Pages struct {
		Connect    string `json:"connect"`
		Login      string `json:"login"`
		LoginError string `json:"loginError"`
	} `json:"pages"`
}

func loadGoldens(t *testing.T) goldens {
	t.Helper()
	data, err := os.ReadFile("testdata/goldens.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldens
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

// IDNA-only spellings are the one kept difference (go-server note).
var idnaOnly = map[string]bool{"https://ｃｌａｕｄｅ.ai/x": true}

func TestCIMDMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).CIMD {
		got := IsCIMDClientID(c.Input)
		if idnaOnly[c.Input] {
			if got {
				t.Errorf("%q: IDNA spelling accepted", c.Input)
			}
			continue
		}
		if got != c.CIMD {
			t.Errorf("%q: got %v, Node %v", c.Input, got, c.CIMD)
		}
	}
}

func TestDCRMatchesNode(t *testing.T) {
	p := &PublicClients{SigningSecret: func() (string, error) { return "golden-secret", nil }}
	for _, c := range loadGoldens(t).DCR {
		id, err := p.IssueDCRClientID(c.URIs, c.IssuedAt)
		if err != nil || id != c.ClientID {
			t.Errorf("issue %v: got %q (%v), Node %q", c.URIs, id, err, c.ClientID)
		}
		uris, ok, err := p.VerifyDCRClientID(c.ClientID)
		if err != nil || !ok || !reflect.DeepEqual(uris, c.Verified.RedirectURIs) {
			t.Errorf("verify %q: got %v %v %v", c.ClientID, uris, ok, err)
		}
	}
}

// normalize turns []string into []any so values compare with decoded JSON.
func normalize(p httpapi.Params) map[string]any {
	out := map[string]any{}
	for k, v := range p {
		if list, ok := v.([]string); ok {
			anyList := make([]any, len(list))
			for i, s := range list {
				anyList[i] = s
			}
			out[k] = anyList
			continue
		}
		out[k] = v
	}
	return out
}

func isIndexKey(k string) bool {
	if k == "" {
		return false
	}
	for _, c := range k {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func TestFormMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Form {
		req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(c.Input))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		got, ok, err := httpapi.ReadForm(req)
		if err != nil || !ok {
			t.Fatalf("%q: %v %v", c.Input, ok, err)
		}
		g := normalize(got)
		// Index keys ("0") come from bracket forms no route reads.
		for k := range c.Parsed {
			if isIndexKey(k) {
				delete(c.Parsed, k)
			}
		}
		for k := range g {
			if isIndexKey(k) {
				delete(g, k)
			}
		}
		if !reflect.DeepEqual(g, c.Parsed) {
			t.Errorf("%q: got %#v, Node %#v", c.Input, g, c.Parsed)
		}
	}
}

func TestQueryMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Query {
		req := httptest.NewRequest(http.MethodGet, "/authorize?"+c.Input, nil)
		got := normalize(httpapi.Query(req))
		if !reflect.DeepEqual(got, c.Parsed) {
			t.Errorf("%q: got %#v, Node %#v", c.Input, got, c.Parsed)
		}
	}
}

func TestContinueURLMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).ContinueURLs {
		got := "/authorize?" + serializeSearch(setSearch(parseSearch(c.Input), "confirmed", "1"))
		if got != c.URL {
			t.Errorf("%q: got %q, Node %q", c.Input, got, c.URL)
		}
	}
}

func TestNegotiationMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Negotiation {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		label := "<none>"
		if c.Accept != nil {
			req.Header.Set("Accept", *c.Accept)
			label = *c.Accept
		}
		if got := negotiateTextHTML(req); got != c.Chosen {
			t.Errorf("Accept %q: got %q, Node %q", label, got, c.Chosen)
		}
	}
}

func TestPagesMatchNode(t *testing.T) {
	g := loadGoldens(t).Pages
	if got := renderConnectPage("<img src=x onerror=alert(1)>", "/authorize?a=1&b=%22"); got != g.Connect {
		t.Errorf("connect page differs:\n%s", firstDiff(got, g.Connect))
	}
	login := renderLoginPage("claude.ai", []oauthField{
		{"response_type", "code"},
		{"client_id", "https://claude.ai/oauth/mcp-oauth-client-metadata"},
		{"redirect_uri", "https://claude.ai/api/mcp/auth_callback"},
		{"state", `"><script>alert(1)</script>`},
		{"code_challenge", "abc"},
		{"code_challenge_method", []string{"S256", "x"}},
	}, "", "")
	if login != g.Login {
		t.Errorf("login page differs:\n%s", firstDiff(login, g.Login))
	}
	loginErr := renderLoginPage("claude.com", []oauthField{
		{"response_type", "code"}, {"client_id", ""}, {"redirect_uri", nil}, {"state", nil},
		{"code_challenge", nil}, {"code_challenge_method", nil},
	}, "o'brien & co", "Wrong agent name or password.")
	if loginErr != g.LoginError {
		t.Errorf("login error page differs:\n%s", firstDiff(loginErr, g.LoginError))
	}
}

func firstDiff(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			start := i - 40
			if start < 0 {
				start = 0
			}
			return "go:   " + a[start:min(len(a), i+40)] + "\nnode: " + b[start:min(len(b), i+40)]
		}
	}
	return "lengths differ"
}
