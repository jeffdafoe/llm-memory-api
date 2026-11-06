package staleness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// lookupTimeout bounds one endoflife.date request.
const lookupTimeout = 15 * time.Second

// Checker gathers one run's facts, as scripts/staleness-check.js did, and
// mails the report. Its fields are the outside world, so tests can replace
// them; New gives the real ones.
type Checker struct {
	// Run runs a command and returns its stdout and stderr. err is
	// *exec.ExitError for a non-zero exit, exec.ErrNotFound (wrapped) when it
	// could not start.
	Run func(ctx context.Context, env []string, name string, args ...string) (stdout, stderr []byte, err error)
	// Root is prefixed to /etc, /var/run and /boot ("" on the server).
	Root string
	// Release is the running kernel's release (uname -r).
	Release  func() (string, error)
	Hostname func() (string, error)
	HTTP     *http.Client
	// EOLBase is endoflife.date's API base, https://endoflife.date/api.
	EOLBase string
	// PostgresVersionNum is SHOW server_version_num.
	PostgresVersionNum func(ctx context.Context) (string, error)
	// Send mails the report from `system` and returns the mail id.
	Send func(ctx context.Context, to, subject, body string) (string, error)
	Now  func() time.Time
	Out  io.Writer
}

// New returns a Checker on the real system. pgVersion and send come from
// the caller, which owns the database.
func New(pgVersion func(ctx context.Context) (string, error), send func(ctx context.Context, to, subject, body string) (string, error)) *Checker {
	return &Checker{
		Run:                runCommand,
		Release:            kernelRelease,
		Hostname:           os.Hostname,
		HTTP:               &http.Client{},
		EOLBase:            "https://endoflife.date/api",
		PostgresVersionNum: pgVersion,
		Send:               send,
		Now:                time.Now,
		Out:                os.Stdout,
	}
}

func runCommand(ctx context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// kernelRelease is os.release() on Linux.
func kernelRelease() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func (c *Checker) path(p string) string {
	if c.Root == "" {
		return p
	}
	return filepath.Join(c.Root, filepath.FromSlash(p))
}

// Stamp writes "staleness-check: <ISO time> message", the script's log line.
func (c *Checker) Stamp(message string) {
	fmt.Fprintf(c.Out, "staleness-check: %s %s\n", isoMillis(c.Now()), message)
}

type runtimeInfo struct {
	label, product, cycle string
}

// commandFailure is the text Node's execFileSync error gave for a command
// that failed: for one run with its stderr piped and no encoding, String()
// of the stderr Buffer, which is truthy even when empty; for one run with
// an encoding, the stderr text, else "Command failed: <command>"; for one
// that could not start, "spawnSync <name> <CODE>" (ENOENT when it is not on
// PATH or its interpreter is missing, EACCES when it is not executable).
func commandFailure(err error, stderr []byte, bufferStderr bool, command string) string {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		name := strings.Fields(command)[0]
		if errors.Is(err, exec.ErrNotFound) {
			return "spawnSync " + name + " ENOENT"
		}
		if code, _, ok := errnoText(err); ok {
			return "spawnSync " + name + " " + code
		}
		return err.Error()
	}
	text := string(stderr)
	if bufferStderr || text != "" {
		return jsutil.Trim(text)
	}
	return "Command failed: " + command
}

var digits = regexp.MustCompile(`^\d+$`)

// Check runs every check and mails the report to recipient, or logs that
// there was nothing to report. A check that fails becomes a line of the
// report; the error is for a failure of the run itself (the mail send).
func (c *Checker) Check(ctx context.Context, recipient string) error {
	today := c.Now()
	var failures []string
	apt := Apt{}

	// Refreshing the lists is what makes the simulation current; it
	// changes no installed package.
	if _, stderr, err := c.Run(ctx, os.Environ(), "apt-get", "update", "-qq"); err != nil {
		failures = append(failures, "`apt-get update` failed, so the update list below may be out of date: "+commandFailure(err, stderr, true, "apt-get update -qq"))
	}
	stdout, stderr, err := c.Run(ctx, append(os.Environ(), "LC_ALL=C"), "apt-get", "-s", "dist-upgrade")
	if err != nil {
		failures = append(failures, "`apt-get -s dist-upgrade` failed: "+commandFailure(err, stderr, false, "apt-get -s dist-upgrade"))
	} else {
		apt = ParseAptSimulation(string(stdout))
		for _, problem := range apt.Problems {
			failures = append(failures, "Reading the apt simulation: "+problem)
		}
	}

	// The running versions, read from the system itself rather than
	// config, so the check reports what is actually installed. Node also
	// checked its own runtime (Node.js); the Go binary has none to check.
	var runtimes []runtimeInfo
	if data, err := os.ReadFile(c.path("/etc/debian_version")); err != nil {
		failures = append(failures, "Could not read the Debian version: "+fsMessage(err, "open", "/etc/debian_version"))
	} else {
		version := jsutil.Trim(string(data))
		major, _, _ := strings.Cut(version, ".")
		if !digits.MatchString(major) {
			failures = append(failures, "Could not read the Debian version: not a numbered release: "+version)
		} else {
			runtimes = append(runtimes, runtimeInfo{"Debian", "debian", major})
		}
	}
	if num, err := c.PostgresVersionNum(ctx); err != nil {
		failures = append(failures, "Could not read the PostgreSQL version: "+pgMessage(err))
	} else {
		// server_version_num is e.g. 170006 for 17.6.
		runtimes = append(runtimes, runtimeInfo{"PostgreSQL", "postgresql", jsutil.NumberString(math.Floor(jsutil.StringToNumber(num) / 10000))})
	}

	reboot, err := c.reboot()
	if err != nil {
		failures = append(failures, "Could not check whether a reboot is needed: "+err.Error())
	}

	certs := CollectCertificates(c.path("/etc/letsencrypt"), today)
	failures = append(failures, certs.Failures...)

	var eolFindings []EOLFinding
	for _, rt := range runtimes {
		finding, err := c.lookupEOL(ctx, rt, today)
		if err != nil {
			failures = append(failures, fmt.Sprintf("endoflife.date lookup for %s %s failed: %s", rt.label, rt.cycle, err))
			continue
		}
		if finding != nil {
			eolFindings = append(eolFindings, *finding)
		}
	}

	host, err := c.Hostname()
	if err != nil {
		return err
	}
	report := BuildReport(ReportInput{
		Host:         host,
		Upgrades:     apt.Upgrades,
		Removals:     apt.Removals,
		HeldBack:     apt.HeldBack,
		EOLFindings:  eolFindings,
		Reboot:       reboot,
		CertFindings: certs.Findings,
		Failures:     failures,
	})
	var checked []string
	for _, rt := range runtimes {
		checked = append(checked, rt.label+" "+rt.cycle)
	}
	certWord := "TLS certificates"
	if certs.Checked == 1 {
		certWord = "TLS certificate"
	}
	checked = append(checked, fmt.Sprintf("%d %s", certs.Checked, certWord))
	if report == nil {
		c.Stamp(fmt.Sprintf("nothing to report (0 pending upgrades; checked %s)", strings.Join(checked, ", ")))
		return nil
	}
	id, err := c.Send(ctx, recipient, report.Subject, report.Body)
	if err != nil {
		return err
	}
	c.Stamp(fmt.Sprintf("mailed %s (%s): %s", recipient, id, report.Subject))
	return nil
}

// reboot gathers the reboot facts: the flag file Debian packages may leave
// (and its .pkgs list beside it), the running kernel, and the kernels
// installed in /boot (vmlinuz-<release>).
func (c *Checker) reboot() (*Reboot, error) {
	exists := func(p string) bool {
		_, err := os.Stat(c.path(p))
		return err == nil
	}
	in := RebootInput{FlagExists: exists("/var/run/reboot-required")}
	if in.FlagExists && exists("/var/run/reboot-required.pkgs") {
		data, err := os.ReadFile(c.path("/var/run/reboot-required.pkgs"))
		if err != nil {
			return nil, errors.New(fsMessage(err, "open", "/var/run/reboot-required.pkgs"))
		}
		in.FlagPackages = string(data)
	}
	entries, err := os.ReadDir(c.path("/boot"))
	if err != nil {
		return nil, errors.New(fsMessage(err, "scandir", "/boot"))
	}
	for _, e := range entries {
		if name, ok := strings.CutPrefix(e.Name(), "vmlinuz-"); ok {
			in.InstalledKernels = append(in.InstalledKernels, name)
		}
	}
	release, err := c.Release()
	if err != nil {
		return nil, err
	}
	in.RunningKernel = release
	return AssessReboot(in), nil
}

// lookupEOL asks endoflife.date about one release cycle (nodejs/24) and
// assesses its `eol` field. The error texts are Node's fetch errors.
func (c *Checker) lookupEOL(ctx context.Context, rt runtimeInfo, today time.Time) (*EOLFinding, error) {
	ctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	url := fmt.Sprintf("%s/%s/%s.json", c.EOLBase, rt.product, encodeURIComponent(rt.cycle))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("The operation was aborted due to timeout")
		}
		return nil, errors.New("fetch failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("The operation was aborted due to timeout")
		}
		return nil, errors.New("fetch failed")
	}
	var body any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	// Node read body.eol: a null body threw, any other value without an
	// eol key read as undefined.
	if body == nil {
		return nil, errors.New("Cannot read properties of null (reading 'eol')")
	}
	var eol []byte
	if obj, ok := body.(map[string]any); ok {
		if v, ok := obj["eol"]; ok {
			eol, _ = json.Marshal(v)
		}
	}
	return AssessEOL(rt.label, rt.cycle, eol, today)
}

// encodeURIComponent is JavaScript's: everything but A-Z a-z 0-9 - _ . ! ~ *
// ' ( ) is percent-encoded as UTF-8.
func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || strings.IndexByte("-_.!~*'()", ch) >= 0 {
			b.WriteByte(ch)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", ch)
	}
	return b.String()
}

// pgMessage is err.message as node-postgres gave it: a Postgres error's
// own text, without pgx's "ERROR: " and SQLSTATE.
func pgMessage(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Message
	}
	return err.Error()
}
