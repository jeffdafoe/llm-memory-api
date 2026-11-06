//go:build contract

package contract

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// mcpClientTimeout bounds one MCP client step.
const mcpClientTimeout = 60 * time.Second

// MCPClientRunner runs MCP client steps against one server: contract/
// mcpclient/mcpclient.js under Node, with the TypeScript SDK client from
// node/api, so both servers face the same real client.
type MCPClientRunner struct {
	Dir    string // node/api, where the SDK is installed
	Script string
	URL    string // the server's /mcp
}

// Run runs one script and returns the client's output lines. A failed run
// is a harness failure; a failed connect or call is part of the output.
func (m *MCPClientRunner) Run(ctx context.Context, script string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, mcpClientTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", m.Script)
	cmd.Dir = m.Dir
	cmd.Env = append(os.Environ(), "MCP_URL="+m.URL)
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w\n--- stdout ---\n%s\n--- stderr ---\n%s", err, tail(stdout.String(), 3000), tail(stderr.String(), 3000))
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}
