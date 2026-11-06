package contract

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/events"
)

// newWSServer is a server with the admin WebSocket: the token "good" is user
// "admin", and POST /fire broadcasts one event whose data is fireData.
func newWSServer(t *testing.T, side, fireData string) *Target {
	t.Helper()
	hub := events.NewHub(func(_ context.Context, token string) (string, bool) {
		return "admin", token == "good"
	}, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if events.IsUpgrade(r) {
			hub.ServeUpgrade(w, r)
			return
		}
		if r.URL.Path == "/fire" {
			hub.Broadcast("fired", map[string]string{"x": fireData})
			w.WriteHeader(204)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(func() {
		hub.Close()
		srv.Close()
	})
	return &Target{Name: side, Side: side, BaseURL: srv.URL, Credentials: map[string]string{"admin-web": "good"}}
}

var wsScenario = Scenario{
	Name: "ws round trip",
	Steps: []Step{
		{WS: &WSStep{Open: true, Header: [][2]string{
			{"Connection", "Upgrade"}, {"Upgrade", "websocket"}, {"Sec-WebSocket-Version", "13"},
			{"Sec-WebSocket-Key", wsKey}, {"Sec-WebSocket-Protocol", "bearer, {{admin-web}}"},
		}}},
		{WS: &WSStep{Read: 1}},
		{Method: "POST", Path: "/fire"},
		{WS: &WSStep{Read: 1}},
		{WS: &WSStep{Ping: true}},
		{WS: &WSStep{Conn: "bad", Open: true, Header: [][2]string{
			{"Connection", "Upgrade"}, {"Upgrade", "websocket"}, {"Sec-WebSocket-Protocol", "bearer, nope"},
		}}},
		{WS: &WSStep{Conn: "gone", Open: true, Target: "/elsewhere", Header: [][2]string{
			{"Connection", "Upgrade"}, {"Upgrade", "websocket"},
		}}},
		{WS: &WSStep{Close: true}},
	},
}

func runWSPair(t *testing.T, nodeData, goData string) Outcome {
	t.Helper()
	f := NewFakeUpstream()
	t.Cleanup(f.Close)
	r := &Runner{Fake: f, QuietFor: 50 * time.Millisecond, SettleLimit: 5 * time.Second}
	out, err := r.Run(context.Background(), wsScenario, newWSServer(t, "node", nodeData), newWSServer(t, "go", goData))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRunnerWebSocketSteps(t *testing.T) {
	out := runWSPair(t, "same", "same")
	if !out.Report.OK() {
		t.Fatalf("identical servers differ:\n%s", out.Report)
	}
	steps := out.Node.Steps
	if got := steps[0].Response.Body.Text; !strings.HasPrefix(got, "HTTP/1.1 101 Switching Protocols\r\n") || !strings.Contains(got, "Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n") {
		t.Errorf("open body %q", got)
	}
	if got := steps[1].Response.Body.JSON.Compact(); got != `[{"event":"connected","data":{"user":"admin"}}]` {
		t.Errorf("welcome %s", got)
	}
	if got := steps[3].Response.Body.JSON.Compact(); got != `[{"event":"fired","data":{"x":"same"}}]` {
		t.Errorf("event %s", got)
	}
	if got := steps[4].Response.Body.Text; got != "pong contract-ping" {
		t.Errorf("ping %q", got)
	}
	if got := steps[5].Response.Body.Text; got != "HTTP/1.1 401 Unauthorized\r\n\r\n<closed>" {
		t.Errorf("refused %q", got)
	}
	if got := steps[6].Response.Body.Text; got != "<closed>" {
		t.Errorf("destroyed %q", got)
	}
	if got := steps[7].Response.Body.Text; got != "close 1000\n<closed>" {
		t.Errorf("close %q", got)
	}
}

func TestRunnerWebSocketDifference(t *testing.T) {
	out := runWSPair(t, "same", "other")
	if out.Report.OK() {
		t.Fatal("a different event payload passed")
	}
	if got := out.Report.String(); !strings.Contains(got, "step 4 ws ws read 1: body.0.data.x") {
		t.Errorf("report:\n%s", got)
	}
}

func TestRunnerWebSocketReadTimeoutFailsTheRun(t *testing.T) {
	f := NewFakeUpstream()
	t.Cleanup(f.Close)
	r := &Runner{Fake: f, QuietFor: 50 * time.Millisecond, SettleLimit: 5 * time.Second}
	sc := Scenario{Name: "read past the end", Steps: []Step{wsScenario.Steps[0], {WS: &WSStep{Read: 2}}}}
	start := time.Now()
	_, err := r.Run(context.Background(), sc, newWSServer(t, "node", "a"), newWSServer(t, "go", "a"))
	if err == nil || !strings.Contains(err.Error(), "after 1 of 2 messages") {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > 2*wsTimeout {
		t.Errorf("took %s", time.Since(start))
	}
}
