package logger

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/actors"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

var fixedNow = time.Date(2026, 11, 4, 19, 43, 29, 143_000_000, time.UTC)

func newTestLogger(t *testing.T, mock pgxmock.PgxPoolIface, logDir string) (*Logger, *bytes.Buffer) {
	t.Helper()
	l := New(mock, actors.NewCache(mock), logDir)
	var out bytes.Buffer
	l.stdout = &out
	l.stderr = &bytes.Buffer{}
	l.now = func() time.Time { return fixedNow }
	return l, &out
}

func TestLogLineFormats(t *testing.T) {
	dir := t.TempDir()
	l, out := newTestLogger(t, testutil.NewMock(t), dir)

	l.Log("watchdog", "systemd-heartbeat-started", Fields{{"watchdogSec", 60}, {"heartbeatMs", 30000}, {"note", "<a & b>"}})

	wantStdout := `[watchdog] 2026-11-04T19:43:29.143Z systemd-heartbeat-started: {"watchdogSec":60,"heartbeatMs":30000,"note":"<a & b>"}` + "\n"
	if out.String() != wantStdout {
		t.Fatalf("stdout:\n got %q\nwant %q", out.String(), wantStdout)
	}

	file, err := os.ReadFile(filepath.Join(dir, "activity.log"))
	if err != nil {
		t.Fatal(err)
	}
	wantFile := `2026-11-04T19:43:29.143Z [watchdog] systemd-heartbeat-started: {"watchdogSec":60,"heartbeatMs":30000,"note":"<a & b>"}` + "\n"
	if string(file) != wantFile {
		t.Fatalf("file:\n got %q\nwant %q", file, wantFile)
	}
}

func TestLogWithoutWritableDirIsStdoutOnly(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	l, out := newTestLogger(t, testutil.NewMock(t), missing)
	if l.logFile != "" {
		t.Fatalf("logFile = %q, want file logging off", l.logFile)
	}
	l.Log("x", "y", nil)
	if out.String() != "[x] 2026-11-04T19:43:29.143Z y: {}\n" {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestLogErrorWritesRowWithNulls(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("work").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name"}).AddRow(4, "work"))
	actorID := 4
	mock.ExpectExec("INSERT INTO error_log").
		WithArgs("mail", "send", &actorID, (*string)(nil), (*string)(nil), "boom", (*string)(nil), (*int)(nil)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	l, out := newTestLogger(t, mock, "")
	l.LogError("mail", "send", ErrorOpts{Agent: "work", Message: "boom"})
	testutil.WaitMet(t, mock)

	want := `[mail] 2026-11-04T19:43:29.143Z send: {"agent":"work","error":"boom"}` + "\n"
	if out.String() != want {
		t.Fatalf("stdout = %q, want %q", out.String(), want)
	}
}

func TestLogErrorUnknownAgentStoresNullActor(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery("SELECT id, name FROM actors WHERE name").WithArgs("ghost").WillReturnError(pgx.ErrNoRows)
	context := "discussion"
	detail := "stack"
	status := 409
	mock.ExpectExec("INSERT INTO error_log").
		WithArgs("route", "label", (*int)(nil), &context, (*string)(nil), "conflict", &detail, &status).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	l, out := newTestLogger(t, mock, "")
	l.LogError("route", "label", ErrorOpts{Agent: "ghost", Context: "discussion", Message: "conflict", Detail: "stack", StatusCode: 409})
	testutil.WaitMet(t, mock)
	if !strings.Contains(out.String(), `{"agent":"ghost","context":"discussion","error":"conflict"}`) {
		t.Fatalf("stdout = %q", out.String())
	}
}
