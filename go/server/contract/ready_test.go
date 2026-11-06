package contract

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestListeningPort(t *testing.T) {
	for _, c := range []struct {
		out  string
		port int
		ok   bool
	}{
		{"Memory API listening on 127.0.0.1:43211\n", 43211, true},
		{"[x] noise\r\nMemory API listening on 127.0.0.1:5\r\n", 5, true},
		{"Memory API listening on 127.0.0.1:0\n", 0, false},
		{"Memory API listening on 127.0.0.1:99999\n", 0, false},
		{"Memory API listening on 0.0.0.0:43211\n", 0, false},
		{"memory-api: listen tcp 127.0.0.1:43211: bind: address already in use\n", 0, false},
		{"Memory API listening on 127.0.0.1:432", 0, false},
	} {
		port, ok := listeningPort(c.out)
		if port != c.port || ok != c.ok {
			t.Errorf("%q: got %d %v want %d %v", c.out, port, ok, c.port, c.ok)
		}
	}
}

// fakeChild is a server process as awaitReady sees it.
type fakeChild struct {
	mu   sync.Mutex
	out  strings.Builder
	done chan struct{}
}

func newFakeChild() *fakeChild { return &fakeChild{done: make(chan struct{})} }

func (f *fakeChild) print(s string) {
	f.mu.Lock()
	f.out.WriteString(s)
	f.mu.Unlock()
}

func (f *fakeChild) readiness() readiness {
	return readiness{
		output:  func() string { f.mu.Lock(); defer f.mu.Unlock(); return f.out.String() },
		done:    f.done,
		exitErr: func() error { return errors.New("exit status 1") },
		healthy: healthOK,
	}
}

func okServer(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.Close)
	return s
}

func portOf(s *httptest.Server) string {
	return s.URL[strings.LastIndex(s.URL, ":")+1:]
}

// An unrelated server answering 200 on some port never makes a child ready
// that has not said where it listens; the child's late bind failure is the
// result.
func TestAwaitReadyIgnoresAnUnrelatedListener(t *testing.T) {
	okServer(t)
	child := newFakeChild()
	go func() {
		time.Sleep(400 * time.Millisecond)
		child.print("memory-api: listen tcp 127.0.0.1:0: bind: no luck\n")
		close(child.done)
	}()
	_, err := awaitReady(context.Background(), child.readiness(), 5*time.Second)
	if err == nil || !strings.HasPrefix(err.Error(), "exited:") {
		t.Fatalf("err = %v, want the child's exit", err)
	}
}

// The port comes from the child's own line, so it is the child that
// answers.
func TestAwaitReadyUsesTheReportedPort(t *testing.T) {
	okServer(t)
	mine := okServer(t)
	child := newFakeChild()
	go func() {
		time.Sleep(200 * time.Millisecond)
		child.print("Memory API listening on 127.0.0.1:" + portOf(mine) + "\n")
	}()
	url, err := awaitReady(context.Background(), child.readiness(), 5*time.Second)
	if err != nil || url != mine.URL {
		t.Fatalf("got %q %v, want %q", url, err, mine.URL)
	}
}

// A child that dies after its line does not count as ready, even if
// something answers on the port it named.
func TestAwaitReadyRefusesAChildThatExited(t *testing.T) {
	other := okServer(t)
	child := newFakeChild()
	child.print("Memory API listening on 127.0.0.1:" + portOf(other) + "\n")
	close(child.done)
	if _, err := awaitReady(context.Background(), child.readiness(), 5*time.Second); err == nil {
		t.Fatal("a child that exited was reported ready")
	}
}

func TestAwaitReadyTimesOut(t *testing.T) {
	child := newFakeChild()
	_, err := awaitReady(context.Background(), child.readiness(), 300*time.Millisecond)
	if err == nil || err.Error() != "no listening line in time" {
		t.Fatalf("err = %v", err)
	}
}
