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

var wsUpgradeHeader = [][2]string{
	{"Connection", "Upgrade"}, {"Upgrade", "websocket"}, {"Sec-WebSocket-Version", "13"},
	{"Sec-WebSocket-Key", wsKey}, {"Sec-WebSocket-Protocol", "bearer, {{admin-web}}"},
}

var wsScenario = Scenario{
	Name: "ws round trip",
	Steps: []Step{
		{WS: &WSStep{Open: true, Expect: "101", Header: wsUpgradeHeader}},
		{WS: &WSStep{Read: 1}},
		{Method: "POST", Path: "/fire"},
		{WS: &WSStep{Read: 1}},
		{WS: &WSStep{Ping: true}},
		{WS: &WSStep{Conn: "bad", Open: true, Expect: "401", Header: [][2]string{
			{"Connection", "Upgrade"}, {"Upgrade", "websocket"}, {"Sec-WebSocket-Protocol", "bearer, nope"},
		}}},
		{WS: &WSStep{Conn: "gone", Open: true, Expect: "closed", Target: "/elsewhere", Header: [][2]string{
			{"Connection", "Upgrade"}, {"Upgrade", "websocket"},
		}}},
		{WS: &WSStep{Close: true, CloseCode: 4001, CloseReason: "bye"}},
	},
}

func newRunner(t *testing.T) *Runner {
	t.Helper()
	f := NewFakeUpstream()
	t.Cleanup(f.Close)
	return &Runner{Fake: f, QuietFor: 50 * time.Millisecond, SettleLimit: 5 * time.Second}
}

func runWSPair(t *testing.T, nodeData, goData string) Outcome {
	t.Helper()
	out, err := newRunner(t).Run(context.Background(), wsScenario, newWSServer(t, "node", nodeData), newWSServer(t, "go", goData))
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
	if got := steps[1].Response.Body.JSON.Compact(); got != `["{\"event\":\"connected\",\"data\":{\"user\":\"admin\"}}"]` {
		t.Errorf("welcome %s", got)
	}
	if got := steps[3].Response.Body.JSON.Compact(); got != `["{\"event\":\"fired\",\"data\":{\"x\":\"same\"}}"]` {
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
	// The hub echoes the code and the reason, as ws does.
	if got := steps[7].Response.Body.Text; got != "close 4001 \"bye\"\n<closed>" {
		t.Errorf("close %q", got)
	}
}

func TestRunnerWebSocketDifference(t *testing.T) {
	out := runWSPair(t, "same", "other")
	if out.Report.OK() {
		t.Fatal("a different event payload passed")
	}
	if got := out.Report.String(); !strings.Contains(got, "step 4 ws ws read 1: body.0") {
		t.Errorf("report:\n%s", got)
	}
}

// An open whose outcome is not the expected one fails the run, even when
// both sides agree.
func TestRunnerWebSocketExpectFailsTheRun(t *testing.T) {
	sc := Scenario{Name: "expects an upgrade", Steps: []Step{{WS: &WSStep{Open: true, Expect: "101", Header: [][2]string{
		{"Connection", "Upgrade"}, {"Upgrade", "websocket"}, {"Sec-WebSocket-Protocol", "bearer, nope"},
	}}}}}
	_, err := newRunner(t).Run(context.Background(), sc, newWSServer(t, "node", "a"), newWSServer(t, "go", "a"))
	if err == nil || !strings.Contains(err.Error(), "expected 101, got 401") {
		t.Fatalf("err %v", err)
	}
	sc.Steps[0].WS.Expect = ""
	if _, err := newRunner(t).Run(context.Background(), sc, newWSServer(t, "node", "a"), newWSServer(t, "go", "a")); err == nil || !strings.Contains(err.Error(), "needs Expect") {
		t.Fatalf("no Expect: err %v", err)
	}
}

func TestRunnerWebSocketReadTimeoutFailsTheRun(t *testing.T) {
	sc := Scenario{Name: "read past the end", Steps: []Step{wsScenario.Steps[0], {WS: &WSStep{Read: 2}}}}
	start := time.Now()
	_, err := newRunner(t).Run(context.Background(), sc, newWSServer(t, "node", "a"), newWSServer(t, "go", "a"))
	if err == nil || !strings.Contains(err.Error(), "after 1 of 2 messages") {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > 2*wsTimeout {
		t.Errorf("took %s", time.Since(start))
	}
}

// newRawWSServer answers any upgrade with a fixed 101 and then writes frames
// as given, so a test can send what a correct server never would.
func newRawWSServer(t *testing.T, side string, frames []byte) *Target {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		buf.Write(frames)
		buf.Flush()
		time.Sleep(200 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	return &Target{Name: side, Side: side, BaseURL: srv.URL}
}

// textFrame is one unmasked final text frame of a short payload.
func textFrame(payload string) []byte {
	return append([]byte{0x81, byte(len(payload))}, payload...)
}

func rawRead(t *testing.T, nodeFrames, goFrames []byte) (Outcome, error) {
	t.Helper()
	sc := Scenario{Name: "raw frames", Steps: []Step{
		{WS: &WSStep{Open: true, Expect: "101", Header: [][2]string{{"Connection", "Upgrade"}, {"Upgrade", "websocket"}}}},
		{WS: &WSStep{Read: 1}},
	}}
	return newRunner(t).Run(context.Background(), sc, newRawWSServer(t, "node", nodeFrames), newRawWSServer(t, "go", goFrames))
}

// The same JSON written differently is a difference: the payload bytes are
// the contract, not only the value they encode.
func TestRunnerWebSocketComparesPayloadBytes(t *testing.T) {
	out, err := rawRead(t, textFrame(`{"a":1,"b":"<"}`), textFrame(`{"a": 1,"b":"<"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out.Report.OK() {
		t.Fatal("two serializations of one value compared equal")
	}
	// A fragmented message is reassembled.
	frag := append([]byte{0x01, 3}, `{"a`...)
	frag = append(frag, 0x80, 4)
	frag = append(frag, `":1}`...)
	out, err = rawRead(t, frag, textFrame(`{"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Report.OK() {
		t.Errorf("a fragmented message differs:\n%s", out.Report)
	}
}

func TestRunnerWebSocketRefusesBadFraming(t *testing.T) {
	good := textFrame(`{"a":1}`)
	masked := []byte{0x81, 0x80 | 7, 0, 0, 0, 0}
	masked = append(masked, `{"a":1}`...)
	rsv := append([]byte{0xC1, 7}, `{"a":1}`...)
	continuation := append([]byte{0x80, 7}, `{"a":1}`...)
	textInText := append(append([]byte{0x01, 2}, `{"`...), textFrame(`{"a":1}`)...)
	longPing := append([]byte{0x89, 126, 0, 126}, make([]byte, 126)...)
	oversizedForm := append([]byte{0x81, 126, 0, 7}, `{"a":1}`...)
	notJSON := textFrame(`not json`)
	for name, frames := range map[string][]byte{
		"masked":                 masked,
		"rsv":                    rsv,
		"continuation first":     continuation,
		"text inside a fragment": textInText,
		"control over 125":       longPing,
		"long length form":       oversizedForm,
		"not json":               notJSON,
		"opcode 3":               append([]byte{0x83, 7}, `{"a":1}`...),
	} {
		if _, err := rawRead(t, frames, good); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
