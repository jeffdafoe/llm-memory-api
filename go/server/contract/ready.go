package contract

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// Each server is started with PORT=0, so the OS gives it a free port at
// bind time, and the harness learns the port from the server's own start
// line. Nothing is reserved and released first, so a server of a parallel
// case can never take the port in between, and a /health answer always
// comes from the process that printed the line.

// listeningLine is the line both servers print once they are bound:
// node/api/src/server.js and go/server/cmd/memory-api print
// "Memory API listening on <host>:<bound port>". The newline is required:
// the output can be read in the middle of a write, and "…:432" may still
// become "…:43211".
var listeningLine = regexp.MustCompile(`(?m)^Memory API listening on 127\.0\.0\.1:(\d+)\r?\n`)

// listeningPort is the bound port from a server's output so far.
func listeningPort(out string) (int, bool) {
	m := listeningLine.FindStringSubmatch(out)
	if m == nil {
		return 0, false
	}
	port, err := strconv.Atoi(m[1])
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

// readiness is what awaitReady watches: the server's output so far, a
// channel closed when the process exits and its exit error, and the
// /health probe.
type readiness struct {
	output  func() string
	done    <-chan struct{}
	exitErr func() error
	healthy func(ctx context.Context, baseURL string) bool
}

// awaitReady waits for the server's start line, then for a 200 from
// /health on the port it named, and returns its base URL. A server that
// exits first, or does not get there within limit, is an error.
func awaitReady(ctx context.Context, r readiness, limit time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	baseURL := ""
	for {
		if baseURL == "" {
			if port, ok := listeningPort(r.output()); ok {
				baseURL = "http://127.0.0.1:" + strconv.Itoa(port)
			}
		}
		if baseURL != "" && r.healthy(ctx, baseURL) {
			// A server that bound the port and then died frees it; the
			// exit check after the 200 keeps a later owner's answer from
			// counting.
			select {
			case <-r.done:
				return "", fmt.Errorf("exited: %v", r.exitErr())
			default:
			}
			return baseURL, nil
		}
		select {
		case <-r.done:
			return "", fmt.Errorf("exited: %v", r.exitErr())
		case <-ctx.Done():
			if baseURL == "" {
				return "", errors.New("no listening line in time")
			}
			return "", errors.New("no 200 from /health in time")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// healthOK is the /health probe.
func healthOK(ctx context.Context, baseURL string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
