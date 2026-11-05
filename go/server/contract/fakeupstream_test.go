package contract

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"
)

func post(t *testing.T, url, body string, header map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func baseFor(t *testing.T, f *FakeUpstream, side, envName string) string {
	t.Helper()
	for _, kv := range f.Env(side) {
		if k, v, _ := strings.Cut(kv, "="); k == envName {
			return v
		}
	}
	t.Fatalf("no %s in Env", envName)
	return ""
}

func TestFakeEnvCoversEveryUpstream(t *testing.T) {
	f := NewFakeUpstream()
	defer f.Close()
	env := strings.Join(f.Env("node"), "\n")
	for _, name := range []string{"ANTHROPIC_API_BASE", "OPENAI_API_BASE", "GOOGLE_API_BASE", "OPENROUTER_API_BASE", "PERPLEXITY_API_BASE", "XAI_API_BASE"} {
		if !strings.Contains(env, name+"="+f.URL()+"/node/") {
			t.Errorf("Env has no %s for side node:\n%s", name, env)
		}
	}
}

// Each default reply carries the fields the Node provider reads.
func TestFakeDefaultReplies(t *testing.T) {
	f := NewFakeUpstream()
	defer f.Close()
	cases := []struct {
		env, path, field, want string
	}{
		{"ANTHROPIC_API_BASE", "/v1/messages", "content.0.text", "fake anthropic reply"},
		{"OPENAI_API_BASE", "/v1/responses", "output.0.content.0.text", "fake openai reply"},
		{"OPENAI_API_BASE", "/v1/chat/completions", "choices.0.message.content", "fake openai reply"},
		{"XAI_API_BASE", "/v1/responses", "output.0.content.0.text", "fake xai reply"},
		{"OPENROUTER_API_BASE", "/api/v1/chat/completions", "choices.0.message.content", "fake openrouter reply"},
		{"PERPLEXITY_API_BASE", "/chat/completions", "choices.0.message.content", "fake perplexity reply"},
		{"GOOGLE_API_BASE", "/v1beta/models/gemini-x:generateContent", "candidates.0.content.parts.0.text", "fake google reply"},
	}
	for _, c := range cases {
		status, body := post(t, baseFor(t, f, "node", c.env)+c.path, `{"model":"m1"}`, nil)
		if status != 200 {
			t.Errorf("%s %s: status %d %s", c.env, c.path, status, body)
			continue
		}
		v, err := ParseJSON(body)
		if err != nil {
			t.Errorf("%s %s: %v", c.env, c.path, err)
			continue
		}
		if got, ok := v.Path(c.field); !ok || got.Str != c.want {
			t.Errorf("%s %s: %s = %v, want %q", c.env, c.path, c.field, got, c.want)
		}
	}

	status, _ := post(t, baseFor(t, f, "node", "OPENAI_API_BASE")+"/v1/nothing", `{}`, nil)
	if status != http.StatusNotFound {
		t.Errorf("unknown endpoint: status %d, want 404", status)
	}
}

func TestFakeRecordsPerSide(t *testing.T) {
	f := NewFakeUpstream()
	defer f.Close()
	post(t, baseFor(t, f, "node", "ANTHROPIC_API_BASE")+"/v1/messages", `{"model":"a"}`, map[string]string{"x-api-key": "k"})
	post(t, baseFor(t, f, "go", "GOOGLE_API_BASE")+"/v1beta/models/g:generateContent", `{"x":1}`, map[string]string{"x-goog-api-key": "k"})

	node, goReqs := f.Requests("node"), f.Requests("go")
	if len(node) != 1 || len(goReqs) != 1 {
		t.Fatalf("got %d node and %d go requests", len(node), len(goReqs))
	}
	if n := node[0]; n.Upstream != "anthropic" || n.Path != "/v1/messages" || strings.Join(n.Auth, ",") != "x-api-key" || string(n.Body) != `{"model":"a"}` {
		t.Errorf("node request recorded as %+v", n)
	}
	if g := goReqs[0]; g.Upstream != "google" || g.Path != "/v1beta/models/g:generateContent" || strings.Join(g.Auth, ",") != "x-goog-api-key" {
		t.Errorf("go request recorded as %+v", g)
	}
	if inFlight, last := f.Activity("node"); inFlight != 0 || last.IsZero() {
		t.Errorf("Activity = %d, %v", inFlight, last)
	}
}

// A queued reply goes to every side once, in order, then the default resumes.
func TestFakeQueueIsCopiedPerSide(t *testing.T) {
	f := NewFakeUpstream()
	defer f.Close()
	f.Env("node")
	f.Queue("anthropic", "/v1/messages", JSONReply(429, `{"error":"slow down"}`), JSONReply(200, `{"first":true}`))
	f.Env("go") // a side created after the queue gets the queue too

	for _, side := range []string{"node", "go"} {
		url := baseFor(t, f, side, "ANTHROPIC_API_BASE") + "/v1/messages"
		if status, _ := post(t, url, `{}`, nil); status != 429 {
			t.Errorf("%s first reply: status %d, want 429", side, status)
		}
		if _, body := post(t, url, `{}`, nil); string(body) != `{"first":true}` {
			t.Errorf("%s second reply: %s", side, body)
		}
		if _, body := post(t, url, `{}`, nil); !strings.Contains(string(body), "fake anthropic reply") {
			t.Errorf("%s third reply should be the default: %s", side, body)
		}
	}
}

func TestFakeEmbeddings(t *testing.T) {
	f := NewFakeUpstream()
	defer f.Close()
	url := baseFor(t, f, "node", "OPENAI_API_BASE") + "/v1/embeddings"

	_, one := post(t, url, `{"model":"text-embedding-3-small","input":"hello"}`, nil)
	_, many := post(t, url, `{"model":"text-embedding-3-small","input":["hello","world"]}`, nil)

	var a, b struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(one, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(many, &b); err != nil {
		t.Fatal(err)
	}
	if len(a.Data) != 1 || len(b.Data) != 2 || b.Data[1].Index != 1 {
		t.Fatalf("wrong shape: %d and %d items", len(a.Data), len(b.Data))
	}
	want := EmbeddingFor("hello")
	for i, x := range a.Data[0].Embedding {
		if x != want[i] || b.Data[0].Embedding[i] != want[i] {
			t.Fatalf("value %d does not round-trip: %v %v %v", i, x, b.Data[0].Embedding[i], want[i])
		}
	}
	if len(want) != EmbeddingDims {
		t.Fatalf("%d dims", len(want))
	}
	var sum float64
	for _, x := range want {
		sum += float64(x) * float64(x)
	}
	if math.Abs(sum-1) > 1e-5 {
		t.Fatalf("not a unit vector: |v|^2 = %v", sum)
	}
	other := EmbeddingFor("world")
	same := true
	for i := range other {
		if other[i] != want[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different texts gave the same vector")
	}
}
