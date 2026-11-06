package contract

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServer is a stand-in for Node or Go: it issues a random session token
// on login, checks it on /v1/me, echoes the request line on /echo, and calls
// the fake upstream on /embed. tweak changes one behavior so a test can make
// the two sides differ.
type fakeServer struct {
	srv      *httptest.Server
	upstream string // OPENAI_API_BASE for this side
	tweak    string

	mu     sync.Mutex
	tokens map[string]bool
}

func newFakeServer(t *testing.T, f *FakeUpstream, side, tweak string) (*fakeServer, *Target) {
	t.Helper()
	s := &fakeServer{upstream: baseFor(t, f, side, "OPENAI_API_BASE"), tweak: tweak, tokens: map[string]bool{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s, &Target{
		Name:        side,
		Side:        side,
		BaseURL:     s.srv.URL,
		Credentials: map[string]string{"agent-a": "fixed-agent-a-token", "agent-a-passphrase": "pass-a"},
	}
}

func (s *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.URL.Path == "/v1/agent/login":
		if !strings.Contains(string(body), `"passphrase":"pass-a"`) {
			w.WriteHeader(403)
			return
		}
		raw := make([]byte, 32)
		rand.Read(raw)
		tok := hex.EncodeToString(raw)
		s.mu.Lock()
		s.tokens[tok] = true
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		fmt.Fprintf(w, `{"session_token":%q,"expires_at":%q}`, tok, time.Now().UTC().Format("2006-01-02T15:04:05.000Z"))
	case r.URL.Path == "/v1/me":
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.mu.Lock()
		ok := s.tokens[tok]
		s.mu.Unlock()
		if s.tweak == "forget-session" {
			ok = false
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if !ok && tok != "fixed-agent-a-token" {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"error":{"code":"FORBIDDEN","message":"Invalid or expired session token"}}`)
			return
		}
		fmt.Fprintf(w, `{"agent":"agent-a","token":%q}`, tok)
	case r.URL.Path == "/embed":
		go func() {
			time.Sleep(50 * time.Millisecond)
			input := "hello"
			if s.tweak == "prompt-drift" {
				input = "hello!"
			}
			resp, err := http.Post(s.upstream+"/v1/embeddings", "application/json", strings.NewReader(`{"model":"m","input":"`+input+`"}`))
			if err == nil {
				resp.Body.Close()
			}
		}()
		w.WriteHeader(202)
	default:
		// The request target as received, so the test sees paths arrive
		// unchanged; 404 like Express's finalhandler.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(404)
		fmt.Fprintf(w, "Cannot %s %s len=%d te=%v", r.Method, r.RequestURI, r.ContentLength, r.TransferEncoding)
	}
}

func runPair(t *testing.T, sc Scenario, nodeTweak, goTweak string) Outcome {
	t.Helper()
	f := NewFakeUpstream()
	t.Cleanup(f.Close)
	_, node := newFakeServer(t, f, "node", nodeTweak)
	_, goTarget := newFakeServer(t, f, "go", goTweak)
	r := &Runner{Fake: f, QuietFor: 100 * time.Millisecond, SettleLimit: 5 * time.Second}
	out, err := r.Run(context.Background(), sc, node, goTarget)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

var loginScenario = Scenario{
	Name: "login then use the session",
	Steps: []Step{
		{
			Method:  "POST",
			Path:    "/v1/agent/login",
			Header:  [][2]string{{"Content-Type", "application/json"}},
			Body:    `{"agent":"agent-a","passphrase":"{{agent-a-passphrase}}"}`,
			Capture: map[string]string{"session": "session_token"},
		},
		{Method: "GET", Path: "/v1/me", Header: [][2]string{{"Authorization", "Bearer {{session}}"}}},
		{Method: "GET", Path: "/v1/me", Auth: "agent-a"},
		{Label: "escaped path", Method: "GET", Path: "/a%20b/%2e%2E?q=1&x=%zz"},
		{Label: "chunked body", Method: "POST", Path: "/chunked", Body: "abc", Chunked: true},
		{Label: "double slash", Method: "GET", Path: "//a//b%20c?x=1"},
	},
}

func TestRunnerIdenticalServers(t *testing.T) {
	out := runPair(t, loginScenario, "", "")
	if !out.Report.OK() {
		t.Fatalf("identical servers differ:\n%s", out.Report)
	}

	login := out.Node.Steps[0].Response
	if got := login.Body.JSON.Compact(); got != `{"session_token":"<rand-1>","expires_at":"<ts>"}` {
		t.Errorf("login body normalized to %s", got)
	}
	// The token captured on login and sent back is the same <rand-1>.
	if got := out.Node.Steps[1].Response.Body.JSON.Compact(); got != `{"agent":"agent-a","token":"<rand-1>"}` {
		t.Errorf("session reuse normalized to %s", got)
	}
	if got := out.Node.Steps[3].Response.Body.Text; got != "Cannot GET /a%20b/%2e%2E?q=1&x=%zz len=0 te=[]" {
		t.Errorf("escaped path arrived as %q", got)
	}
	if got := out.Node.Steps[4].Response.Body.Text; got != "Cannot POST /chunked len=-1 te=[chunked]" {
		t.Errorf("chunked body arrived as %q", got)
	}
	// Origin form, not "http://a//b...".
	if got := out.Node.Steps[5].Response.Body.Text; got != "Cannot GET //a//b%20c?x=1 len=0 te=[]" {
		t.Errorf("double-slash path arrived as %q", got)
	}
}

// A deliberate difference in Go must fail with a readable location.
func TestRunnerReportsADeliberateDifference(t *testing.T) {
	out := runPair(t, loginScenario, "", "forget-session")
	if out.Report.OK() {
		t.Fatal("a server that forgets sessions passed")
	}
	got := out.Report.String()
	for _, want := range []string{"step 2 GET /v1/me: status", "node: 200", "go:   403"} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
}

func TestRunnerSettlesAndComparesOutbound(t *testing.T) {
	sc := Scenario{
		Name:  "background provider call",
		Steps: []Step{{Method: "POST", Path: "/embed"}, {Settle: true}},
	}
	out := runPair(t, sc, "", "")
	if !out.Report.OK() {
		t.Fatalf("identical servers differ:\n%s", out.Report)
	}
	if len(out.Node.Outbound) != 1 || out.Node.Outbound[0].Label() != "openai POST /v1/embeddings" {
		t.Fatalf("settle did not wait for the background call: %+v", out.Node.Outbound)
	}

	out = runPair(t, sc, "", "prompt-drift")
	if out.Report.OK() || !strings.Contains(out.Report.String(), "outbound 1 openai POST /v1/embeddings: body.input") {
		t.Fatalf("drift in the upstream request was not reported:\n%s", out.Report)
	}
}

// A harness problem fails the run even when both sides hit it the same way;
// compared as results, two identical failures would pass.
func TestRunnerHarnessErrorsFailTheRun(t *testing.T) {
	cases := []struct {
		name string
		step Step
		want string
	}{
		{"undefined value", Step{Method: "GET", Path: "/v1/me", Header: [][2]string{{"Authorization", "Bearer {{nothing}}"}}}, "{{nothing}}"},
		{"unknown credential", Step{Method: "GET", Path: "/v1/me", Auth: "nobody"}, `no credential "nobody"`},
		{"capture not in response", Step{Method: "GET", Path: "/v1/me", Auth: "agent-a", Capture: map[string]string{"x": "no.such.field"}}, "capture x"},
		{"capture from a non-JSON response", Step{Method: "GET", Path: "/nowhere", Capture: map[string]string{"x": "a"}}, "not JSON"},
	}
	for _, c := range cases {
		f := NewFakeUpstream()
		_, node := newFakeServer(t, f, "node", "")
		_, goTarget := newFakeServer(t, f, "go", "")
		r := &Runner{Fake: f, QuietFor: 50 * time.Millisecond, SettleLimit: 5 * time.Second}
		_, err := r.Run(context.Background(), Scenario{Name: c.name, Steps: []Step{c.step}}, node, goTarget)
		f.Close()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", c.name, err, c.want)
		}
	}
}

// Parallel upstream calls that differ only in their auth headers must sort
// by them, so arrival order does not matter.
func TestSortRecordedUsesAuth(t *testing.T) {
	a := Recorded{Upstream: "openai", Method: "POST", Path: "/v1/embeddings", Auth: []string{"authorization"}, Body: []byte(`{"input":"x"}`)}
	b := Recorded{Upstream: "openai", Method: "POST", Path: "/v1/embeddings", Body: []byte(`{"input":"x"}`)}
	first := sortRecorded(NewNormalizer(), []Recorded{a, b})
	second := sortRecorded(NewNormalizer(), []Recorded{b, a})
	for i := range first {
		if strings.Join(first[i].Auth, ",") != strings.Join(second[i].Auth, ",") {
			t.Fatalf("arrival order changed the sorted order at %d", i)
		}
	}
}

// A commit stalled on the disk leaves the fingerprint unchanged; settle must
// wait for the busy connection, not end on the quiet fingerprint.
func TestRunnerSettleWaitsForBusyConnections(t *testing.T) {
	f := NewFakeUpstream()
	defer f.Close()
	r := &Runner{Fake: f, QuietFor: 50 * time.Millisecond, SettleLimit: 5 * time.Second}
	start := time.Now()
	busyFor := 400 * time.Millisecond
	target := &Target{Name: "node", Side: "node",
		Fingerprint: func(context.Context) (string, error) { return "request_log=0", nil },
		Busy: func(context.Context) (int, error) {
			if time.Since(start) < busyFor {
				return 1, nil
			}
			return 0, nil
		},
	}
	if err := r.settle(context.Background(), target, Step{Settle: true}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < busyFor {
		t.Fatalf("settle ended after %s, while a connection was still busy", took)
	}
}

func TestRunnerSettleTimeoutNamesWhatChanged(t *testing.T) {
	f := NewFakeUpstream()
	defer f.Close()
	r := &Runner{Fake: f, QuietFor: 50 * time.Millisecond, SettleLimit: 300 * time.Millisecond}
	n := 0
	target := &Target{Name: "node", Side: "node", Fingerprint: func(context.Context) (string, error) {
		n++
		return fmt.Sprintf("request_log=%d", n), nil
	}}
	err := r.settle(context.Background(), target, Step{Settle: true})
	if err == nil || !strings.Contains(err.Error(), "request_log=") {
		t.Fatalf("err = %v", err)
	}
}
