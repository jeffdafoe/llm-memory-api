//go:build contract

package contract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// BaseURL is what both servers are told their public origin is. It is a
// fixed name, so OAuth metadata and WWW-Authenticate hints read the same on
// both sides without normalizing.
const BaseURL = "https://memory.contract.test"

// serverEnvNames are the variables the servers read
// (node/api/src, go/server/cmd/memory-api). They are removed from the
// inherited environment and set per server, so the developer's own
// environment cannot leak into a run.
var serverEnvNames = []string{
	"DATABASE_URL", "PORT", "MEMORY_API_HOST", "BASE_URL", "LOG_DIR", "OPENAI_API_KEY",
	"NOTIFY_SOCKET", "WATCHDOG_USEC", "NODE_ENV",
}

// ServerProcess is one running server.
type ServerProcess struct {
	Name    string
	BaseURL string
	cmd     *exec.Cmd
	out     *lockedBuffer
	done    chan struct{}
	exitErr error
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// ServerSpec says how to start one server.
type ServerSpec struct {
	Name        string   // "node" or "go"
	Command     []string // program and arguments
	Dir         string
	DatabaseURL string
	Side        string // fake upstream side
	LogDir      string
}

// freePort asks the OS for an unused loopback port. Another process could
// take it before the server binds; the health wait then fails clearly.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func serverEnv(spec ServerSpec, port int, fake *FakeUpstream) []string {
	drop := map[string]bool{}
	for _, n := range serverEnvNames {
		drop[n] = true
	}
	for _, u := range Upstreams {
		drop[u.Env] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !drop[strings.ToUpper(name)] {
			env = append(env, kv)
		}
	}
	env = append(env,
		"DATABASE_URL="+spec.DatabaseURL,
		"PORT="+strconv.Itoa(port),
		"MEMORY_API_HOST=127.0.0.1",
		"BASE_URL="+BaseURL,
		"LOG_DIR="+spec.LogDir,
		// The embeddings call sends it; the fake only checks it is present.
		"OPENAI_API_KEY=contract-dummy-openai-key",
	)
	return append(env, fake.Env(spec.Side)...)
}

// StartServer starts a server and waits until GET /health answers 200.
func StartServer(ctx context.Context, spec ServerSpec, fake *FakeUpstream) (*ServerProcess, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(spec.LogDir, 0o755); err != nil {
		return nil, err
	}
	cmd := exec.Command(spec.Command[0], spec.Command[1:]...)
	cmd.Dir = spec.Dir
	cmd.Env = serverEnv(spec, port, fake)
	out := &lockedBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.Name, err)
	}
	p := &ServerProcess{
		Name:    spec.Name,
		BaseURL: "http://127.0.0.1:" + strconv.Itoa(port),
		cmd:     cmd,
		out:     out,
		done:    make(chan struct{}),
	}
	go func() {
		p.exitErr = cmd.Wait()
		close(p.done)
	}()

	if err := p.waitHealthy(ctx, 60*time.Second); err != nil {
		p.Stop()
		return nil, fmt.Errorf("%s did not start: %w\n--- %s output ---\n%s", spec.Name, err, spec.Name, tail(out.String(), 4000))
	}
	return p, nil
}

func (p *ServerProcess) waitHealthy(ctx context.Context, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second}
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.BaseURL+"/health", nil)
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-p.done:
			return fmt.Errorf("exited: %v", p.exitErr)
		case <-ctx.Done():
			return errors.New("no 200 from /health in time")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Output is what the server wrote to stdout and stderr, for a failed case.
func (p *ServerProcess) Output() string {
	return p.out.String()
}

// Stop kills the server. The results are read before Stop, so there is no
// need for a graceful shutdown, and Windows has no SIGTERM for a child.
func (p *ServerProcess) Stop() {
	select {
	case <-p.done:
		return
	default:
	}
	p.cmd.Process.Kill()
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// BuildGoServer compiles cmd/memory-api into dir and returns the binary.
func BuildGoServer(moduleRoot, dir string) (string, error) {
	bin := filepath.Join(dir, "memory-api")
	if os.PathSeparator == '\\' {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/memory-api")
	cmd.Dir = moduleRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go build: %w\n%s", err, out)
	}
	return bin, nil
}
