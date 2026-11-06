//go:build contract

package contract

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// jobTimeout bounds one job step.
const jobTimeout = 60 * time.Second

// BuildJobRunner compiles contract/runjob, the Go side of the job steps.
// It needs the contract tag, so the server binary never contains it.
func BuildJobRunner(moduleRoot, dir string) (string, error) {
	bin := filepath.Join(dir, "runjob")
	if os.PathSeparator == '\\' {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-tags", "contract", "-o", bin, "./contract/runjob")
	cmd.Dir = moduleRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go build runjob: %w\n%s", err, out)
	}
	return bin, nil
}

// JobRunner runs job steps for one server: a process with that server's
// environment (its database copy and log directory) that runs one pass.
type JobRunner struct {
	Command []string // program and leading arguments; the job name is appended
	Dir     string
	Spec    ServerSpec
	Fake    *FakeUpstream
}

// Run runs one pass and returns the job's log lines (stdout lines of the
// job's subsystem, "[<job>] ..."). A failed run is a harness failure.
func (j *JobRunner) Run(ctx context.Context, job string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, jobTimeout)
	defer cancel()
	args := append(append([]string{}, j.Command[1:]...), job)
	cmd := exec.CommandContext(ctx, j.Command[0], args...)
	cmd.Dir = j.Dir
	cmd.Env = serverEnv(j.Spec, j.Fake)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w\n--- stdout ---\n%s\n--- stderr ---\n%s", err, tail(stdout.String(), 3000), tail(stderr.String(), 3000))
	}
	var lines []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(line, "["+job+"] ") {
			lines = append(lines, strings.TrimRight(line, "\r"))
		}
	}
	return strings.Join(lines, "\n"), nil
}
