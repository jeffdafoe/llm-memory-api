package staleness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestHelperExit is not a test: it is the process a fake command runs when
// it must fail with a real non-zero exit.
func TestHelperExit(t *testing.T) {
	if os.Getenv("STALENESS_HELPER_EXIT") != "1" {
		return
	}
	fmt.Fprint(os.Stderr, os.Getenv("STALENESS_HELPER_STDERR"))
	os.Exit(3)
}

// exitError runs the helper and returns its *exec.ExitError and stderr.
func exitError(t *testing.T, stderr string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperExit$")
	cmd.Env = append(os.Environ(), "STALENESS_HELPER_EXIT=1", "STALENESS_HELPER_STDERR="+stderr)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("helper: %v", err)
	}
	return errBuf.Bytes(), err
}

const cleanApt = "Reading package lists...\n0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n"

type fakeSystem struct {
	t        *testing.T
	root     string
	commands map[string]func() ([]byte, []byte, error)
	eol      map[string]string // path → body; a missing path is 404
	pg       string
	pgErr    error
	sent     []string
	sendErr  error
	out      bytes.Buffer
}

func newFakeSystem(t *testing.T) *fakeSystem {
	f := &fakeSystem{
		t:    t,
		root: t.TempDir(),
		commands: map[string]func() ([]byte, []byte, error){
			"apt-get update -qq":      func() ([]byte, []byte, error) { return nil, nil, nil },
			"apt-get -s dist-upgrade": func() ([]byte, []byte, error) { return []byte(cleanApt), nil, nil },
		},
		eol: map[string]string{
			"/debian/13.json":     `{"cycle":"13","eol":"2028-08-09"}`,
			"/postgresql/17.json": `{"cycle":"17","eol":"2029-11-08"}`,
		},
		pg: "170006",
	}
	f.file("/etc/debian_version", "13.2\n")
	f.dir("/boot")
	return f
}

func (f *fakeSystem) file(path, data string) {
	p := filepath.Join(f.root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeSystem) dir(path string) {
	if err := os.MkdirAll(filepath.Join(f.root, filepath.FromSlash(path)), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeSystem) checker() *Checker {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := f.eol[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, body)
	}))
	f.t.Cleanup(srv.Close)
	return &Checker{
		Run: func(_ context.Context, env []string, name string, args ...string) ([]byte, []byte, error) {
			key := name + " " + strings.Join(args, " ")
			if key == "apt-get -s dist-upgrade" && !contains(env, "LC_ALL=C") {
				f.t.Error("dist-upgrade run without LC_ALL=C")
			}
			run, ok := f.commands[key]
			if !ok {
				return nil, nil, fmt.Errorf("exec: %q: %w", name, exec.ErrNotFound)
			}
			return run()
		},
		Root:               f.root,
		Release:            func() (string, error) { return "6.12.113+deb13-amd64", nil },
		Hostname:           func() (string, error) { return "host5", nil },
		HTTP:               srv.Client(),
		EOLBase:            srv.URL,
		PostgresVersionNum: func(context.Context) (string, error) { return f.pg, f.pgErr },
		Send: func(_ context.Context, to, subject, body string) (string, error) {
			if f.sendErr != nil {
				return "", f.sendErr
			}
			f.sent = append(f.sent, to+"\n"+subject+"\n"+body)
			return "mail-1", nil
		},
		Now: func() time.Time { return time.Date(2026, 10, 21, 12, 30, 0, 0, time.UTC) },
		Out: &f.out,
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestCleanWeekSendsNothing(t *testing.T) {
	f := newFakeSystem(t)
	if err := f.checker().Check(context.Background(), "admin"); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 0 {
		t.Fatalf("sent %q", f.sent)
	}
	want := "staleness-check: 2026-10-21T12:30:00.000Z nothing to report (0 pending upgrades; checked Debian 13, PostgreSQL 17, 0 TLS certificates)\n"
	if f.out.String() != want {
		t.Fatalf("out %q", f.out.String())
	}
}

func TestEverythingWrongIsMailed(t *testing.T) {
	f := newFakeSystem(t)
	updateStderr, updateErr := exitError(t, "")
	f.commands["apt-get update -qq"] = func() ([]byte, []byte, error) { return nil, updateStderr, updateErr }
	f.commands["apt-get -s dist-upgrade"] = func() ([]byte, []byte, error) {
		return []byte("Inst openssl [1] (2 Debian-Security:13/stable-security [amd64])\n0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n"), nil, nil
	}
	f.file("/etc/debian_version", "trixie/sid\n")
	f.pg = "160004"
	delete(f.eol, "/postgresql/17.json")
	f.file("/var/run/reboot-required", "")
	f.file("/var/run/reboot-required.pkgs", "libc6\n")
	f.file("/boot/vmlinuz-6.12.120+deb13-amd64", "")
	f.dir("/etc/letsencrypt/renewal")
	f.file("/etc/letsencrypt/renewal/example.org.conf", "")

	if err := f.checker().Check(context.Background(), "admin"); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 {
		t.Fatalf("sent %d mails", len(f.sent))
	}
	mail := f.sent[0]
	for _, want := range []string{
		"admin\nStaleness check (host5): reboot needed, 1 security update, 5 failed checks\n",
		"- `apt-get update` failed, so the update list below may be out of date: \n",
		"- Reading the apt simulation: apt's summary (0 upgraded, 0 newly installed, 0 to remove) does not match the lines read (1 upgraded, 0 newly installed, 0 to remove)\n",
		"- Could not read the Debian version: not a numbered release: trixie/sid\n",
		"- Could not read the TLS certificate `example.org`: ENOENT: no such file or directory, open '",
		"- endoflife.date lookup for PostgreSQL 16 failed: HTTP 503\n",
		"- Kernel 6.12.120+deb13-amd64 is installed but 6.12.113+deb13-amd64 is running.\n",
		"- Asked for by: `libc6`.\n",
	} {
		if !strings.Contains(mail, want) {
			t.Errorf("mail lacks %q:\n%s", want, mail)
		}
	}
	if want := "staleness-check: 2026-10-21T12:30:00.000Z mailed admin (mail-1): Staleness check (host5): reboot needed, 1 security update, 5 failed checks\n"; f.out.String() != want {
		t.Errorf("out %q", f.out.String())
	}
}

func TestCommandFailureTexts(t *testing.T) {
	f := newFakeSystem(t)
	simStderr, simErr := exitError(t, "")
	f.commands["apt-get -s dist-upgrade"] = func() ([]byte, []byte, error) { return nil, simStderr, simErr }
	delete(f.commands, "apt-get update -qq")
	f.pgErr = &pgconn.PgError{Message: "the database system is starting up"}
	if err := os.RemoveAll(filepath.Join(f.root, "boot")); err != nil {
		t.Fatal(err)
	}
	f.eol["/debian/13.json"] = `{"eol": "soon"}`

	if err := f.checker().Check(context.Background(), "admin"); err != nil {
		t.Fatal(err)
	}
	mail := f.sent[0]
	for _, want := range []string{
		"- `apt-get update` failed, so the update list below may be out of date: spawnSync apt-get ENOENT\n",
		"- `apt-get -s dist-upgrade` failed: Command failed: apt-get -s dist-upgrade\n",
		"- Could not read the PostgreSQL version: the database system is starting up\n",
		"- Could not check whether a reboot is needed: ENOENT: no such file or directory, scandir '/boot'\n",
		"- endoflife.date lookup for Debian 13 failed: unexpected eol value for Debian 13: \"soon\"\n",
	} {
		if !strings.Contains(mail, want) {
			t.Errorf("mail lacks %q:\n%s", want, mail)
		}
	}
}

func TestStderrIsTheFailureText(t *testing.T) {
	stderr, err := exitError(t, "E: Could not get lock /var/lib/dpkg/lock-frontend\n")
	for _, buffered := range []bool{true, false} {
		if got := commandFailure(err, stderr, buffered, "apt-get -s dist-upgrade"); got != "E: Could not get lock /var/lib/dpkg/lock-frontend" {
			t.Errorf("buffered=%v: %q", buffered, got)
		}
	}
}

func TestFailedSendFailsTheRun(t *testing.T) {
	f := newFakeSystem(t)
	f.file("/var/run/reboot-required", "")
	f.sendErr = errors.New(`Agent "admin" not found`)
	err := f.checker().Check(context.Background(), "admin")
	if err == nil || err.Error() != `Agent "admin" not found` {
		t.Fatalf("err %v", err)
	}
}

func TestEncodeURIComponent(t *testing.T) {
	if got := encodeURIComponent("a b/é-_.!~*'()"); got != "a%20b%2F%C3%A9-_.!~*'()" {
		t.Fatalf("%q", got)
	}
}

// Node read body.eol: a null body threw; {} and {"eol": null} are clean.
func TestNullEOLAnswerIsAFailedCheck(t *testing.T) {
	f := newFakeSystem(t)
	f.eol["/debian/13.json"] = "null"
	f.eol["/postgresql/17.json"] = `{"eol": null}`
	if err := f.checker().Check(context.Background(), "admin"); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || !strings.Contains(f.sent[0], "- endoflife.date lookup for Debian 13 failed: Cannot read properties of null (reading 'eol')\n") ||
		strings.Contains(f.sent[0], "PostgreSQL") {
		t.Fatalf("sent %q", f.sent)
	}

	f = newFakeSystem(t)
	f.eol["/debian/13.json"] = "{}"
	f.eol["/postgresql/17.json"] = "[]"
	if err := f.checker().Check(context.Background(), "admin"); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 0 {
		t.Fatalf("sent %q", f.sent)
	}
}

// A command that could not start is "spawnSync <name> <CODE>", whatever
// errno the start failed with.
func TestSpawnFailureTexts(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("exec: %q: %w", "apt-get", exec.ErrNotFound), "spawnSync apt-get ENOENT"},
		{&fs.PathError{Op: "fork/exec", Path: "/usr/bin/apt-get", Err: syscall.ENOENT}, "spawnSync apt-get ENOENT"},
		{&fs.PathError{Op: "fork/exec", Path: "/usr/bin/apt-get", Err: syscall.EACCES}, "spawnSync apt-get EACCES"},
	}
	for _, c := range cases {
		if got := commandFailure(c.err, nil, true, "apt-get update -qq"); got != c.want {
			t.Errorf("%v: %q, want %q", c.err, got, c.want)
		}
	}
}
