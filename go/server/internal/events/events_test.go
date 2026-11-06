package events

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newTestHub serves IsUpgrade requests with a hub that accepts the token
// "good" as user "admin".
func newTestHub(t *testing.T, every time.Duration) (*Hub, *httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var logged []string
	h := NewHub(func(_ context.Context, token string) (string, bool) {
		return "admin", token == "good"
	}, func(format string, args ...any) {
		mu.Lock()
		logged = append(logged, format)
		mu.Unlock()
	})
	h.Every = every
	ctx, cancel := context.WithCancel(context.Background())
	go h.Run(ctx)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsUpgrade(r) {
			h.ServeUpgrade(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(func() {
		cancel()
		h.Close()
		srv.Close()
	})
	return h, srv, &logged
}

func dial(t *testing.T, srv *httptest.Server, protocols ...string) *websocket.Conn {
	t.Helper()
	d := websocket.Dialer{Subprotocols: protocols}
	conn, resp, err := d.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+Path, nil)
	if err != nil {
		t.Fatalf("dial: %v (%v)", err, resp)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func readText(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if kind != websocket.TextMessage {
		t.Fatalf("message type %d", kind)
	}
	return string(msg)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWelcomeThenEventsInOrder(t *testing.T) {
	h, srv, _ := newTestHub(t, time.Hour)
	conn := dial(t, srv, "bearer", "good")
	if got := conn.Subprotocol(); got != "bearer" {
		t.Errorf("subprotocol %q, want the first offered", got)
	}
	if got := readText(t, conn); got != `{"event":"connected","data":{"user":"admin"}}` {
		t.Errorf("welcome %s", got)
	}
	type payload struct {
		Agent  string `json:"agent"`
		Active bool   `json:"active"`
	}
	waitFor(t, "registration", func() bool { return h.Clients() == 1 })
	h.Broadcast("agent_activity", payload{Agent: "a", Active: true})
	h.Broadcast("note_updated", map[string]any(nil))
	h.Broadcast("x", "<&>")
	for _, want := range []string{
		`{"event":"agent_activity","data":{"agent":"a","active":true}}`,
		`{"event":"note_updated","data":null}`,
		`{"event":"x","data":"<&>"}`,
	} {
		if got := readText(t, conn); got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

func TestEveryClientGetsEachEvent(t *testing.T) {
	h, srv, _ := newTestHub(t, time.Hour)
	a := dial(t, srv, "bearer", "good")
	b := dial(t, srv, "bearer", "good")
	readText(t, a)
	readText(t, b)
	waitFor(t, "two clients", func() bool { return h.Clients() == 2 })
	h.Broadcast("e", 1)
	if readText(t, a) != `{"event":"e","data":1}` || readText(t, b) != `{"event":"e","data":1}` {
		t.Error("a client missed the event")
	}
	a.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	waitFor(t, "close to unregister", func() bool { return h.Clients() == 1 })
}

func TestBroadcastWithNoClientEncodesNothing(t *testing.T) {
	h, _, logged := newTestHub(t, time.Hour)
	h.Broadcast("e", make(chan int))
	if len(*logged) != 0 {
		t.Errorf("logged %v", *logged)
	}
}

func TestUnencodableEventIsLogged(t *testing.T) {
	h, srv, logged := newTestHub(t, time.Hour)
	conn := dial(t, srv, "bearer", "good")
	readText(t, conn)
	waitFor(t, "registration", func() bool { return h.Clients() == 1 })
	h.Broadcast("e", make(chan int))
	h.Broadcast("after", true)
	if got := readText(t, conn); got != `{"event":"after","data":true}` {
		t.Errorf("got %s", got)
	}
	if len(*logged) != 1 {
		t.Errorf("logged %v", *logged)
	}
}

// A client that never reads never answers a ping, so the next tick drops it.
func TestSilentClientIsDropped(t *testing.T) {
	h, srv, _ := newTestHub(t, 30*time.Millisecond)
	dial(t, srv, "bearer", "good")
	waitFor(t, "registration", func() bool { return h.Clients() == 1 })
	waitFor(t, "the silent client to be dropped", func() bool { return h.Clients() == 0 })
}

// A client that reads answers pings (gorilla's default handler) and stays.
func TestAnsweringClientStays(t *testing.T) {
	h, srv, _ := newTestHub(t, 20*time.Millisecond)
	conn := dial(t, srv, "bearer", "good")
	var pings int
	var mu sync.Mutex
	conn.SetPingHandler(func(data string) error {
		mu.Lock()
		pings++
		mu.Unlock()
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
	})
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
	waitFor(t, "five pings", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return pings >= 5
	})
	if h.Clients() != 1 {
		t.Errorf("clients = %d after answering every ping", h.Clients())
	}
}

func TestCloseDropsClientsAndRefusesNew(t *testing.T) {
	h, srv, _ := newTestHub(t, time.Hour)
	conn := dial(t, srv, "bearer", "good")
	readText(t, conn)
	waitFor(t, "registration", func() bool { return h.Clients() == 1 })
	h.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Error("connection still open after Close")
	}
	d := websocket.Dialer{Subprotocols: []string{"bearer", "good"}}
	late, _, err := d.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer late.Close()
	late.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := late.ReadMessage(); err == nil {
		t.Error("a connection after Close got a message")
	}
	if h.Clients() != 0 {
		t.Errorf("clients = %d", h.Clients())
	}
}

func TestRefusedTokenIs401(t *testing.T) {
	_, srv, _ := newTestHub(t, time.Hour)
	d := websocket.Dialer{Subprotocols: []string{"bearer", "bad"}}
	_, resp, err := d.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+Path, nil)
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err %v, resp %v", err, resp)
	}
}

func header(pairs ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(pairs); i += 2 {
		h.Add(pairs[i], pairs[i+1])
	}
	return h
}

// The verdicts are Node 24.15's, from a probe server with an 'upgrade'
// listener.
func TestIsUpgrade(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
		want bool
	}{
		{"connection upgrade", header("Connection", "Upgrade", "Upgrade", "websocket"), true},
		{"token among others", header("Connection", "keep-alive, Upgrade", "Upgrade", "websocket"), true},
		{"keep-alive only", header("Connection", "keep-alive", "Upgrade", "websocket"), false},
		{"no connection", header("Upgrade", "websocket"), false},
		{"no upgrade header", header("Connection", "upgrade"), false},
		{"empty upgrade header", header("Connection", "upgrade", "Upgrade", ""), false},
		{"upgradex", header("Connection", "upgradex", "Upgrade", "websocket"), false},
		{"any protocol", header("Connection", "UPGRADE", "Upgrade", "h2c"), true},
		{"second connection line", header("Connection", "keep-alive", "Connection", "upgrade", "Upgrade", "websocket"), true},
		{"spaces", header("Connection", "close,  upgrade ", "Upgrade", "websocket"), true},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/a", nil)
		r.Header = c.h
		if got := IsUpgrade(r); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"bearer":           "",
		"bearer, tok":      "tok",
		"bearer,tok":       "tok",
		" bearer ,  tok  ": "tok",
		"x, bearer, tok":   "tok",
		"tok, bearer":      "",
		"Bearer, tok":      "",
		"bearer, , tok":    "",
	}
	for in, want := range cases {
		r := httptest.NewRequest(http.MethodGet, Path, nil)
		if in != "" {
			r.Header.Set("Sec-WebSocket-Protocol", in)
		}
		if got := bearerToken(r); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// The expectations follow ws 8.22.1's subprotocol.parse.
func TestParseSubprotocols(t *testing.T) {
	ok := map[string][]string{
		"a":            {"a"},
		"a,b":          {"a", "b"},
		"a, b":         {"a", "b"},
		"a ,\tb":       {"a", "b"},
		"bearer, abc_": {"bearer", "abc_"},
	}
	for in, want := range ok {
		got, err := parseSubprotocols(in)
		if err != nil || strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	bad := map[string]string{
		"":      "Unexpected end of input",
		" a":    "Unexpected character at index 0",
		"a ":    "Unexpected end of input",
		"a,":    "Unexpected end of input",
		",a":    "Unexpected character at index 0",
		"a b":   "Unexpected character at index 2",
		"a,a":   `The "a" subprotocol is duplicated`,
		"a/b":   "Unexpected character at index 1",
		"a,,b":  "Unexpected character at index 2",
		"é":     "Unexpected character at index 0",
		"a, b,": "Unexpected end of input",
	}
	for in, want := range bad {
		if _, err := parseSubprotocols(in); err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %s", in, err, want)
		}
	}
}

func TestAbortResponse(t *testing.T) {
	a := &handshakeAbort{code: 400, message: "Missing or invalid Sec-WebSocket-Version header", extra: [][2]string{{"Sec-WebSocket-Version", "13, 8"}}}
	want := "HTTP/1.1 400 Bad Request\r\nConnection: close\r\nContent-Type: text/html\r\nContent-Length: 47\r\nSec-WebSocket-Version: 13, 8\r\n\r\nMissing or invalid Sec-WebSocket-Version header"
	if got := a.response(); got != want {
		t.Errorf("%q", got)
	}
}
