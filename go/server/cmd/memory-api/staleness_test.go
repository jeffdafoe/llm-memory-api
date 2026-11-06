package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestURLFromEnvFile(t *testing.T) {
	cases := []struct {
		in, want, err string
	}{
		{"PORT=3100\nDATABASE_URL=postgres://u:p@h/db\n", "postgres://u:p@h/db", ""},
		{"DATABASE_URL= \"postgres://x\" \r\n", "postgres://x", ""},
		{"DATABASE_URL='postgres://x'", "postgres://x", ""},
		{"DATABASE_URL=\"postgres://x'", "\"postgres://x'", ""},
		{"DATABASE_URL=\"a\"b\"", "a\"b", ""},
		{" DATABASE_URL=x\nOTHER=1", "", "DATABASE_URL not found in /etc/memory-api/env"},
	}
	for _, c := range cases {
		got, err := urlFromEnvFile(c.in)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if got != c.want || msg != c.err {
			t.Errorf("urlFromEnvFile(%q) = %q %q, want %q %q", c.in, got, msg, c.want, c.err)
		}
	}
}

func TestEnvFileErrorsHaveNodeText(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "env")
	_, err := databaseURLFrom(missing)
	if want := "ENOENT: no such file or directory, open '" + missing + "'"; err == nil || err.Error() != want {
		t.Fatalf("missing: %v, want %s", err, want)
	}
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits do not apply")
	}
	locked := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(locked, []byte("DATABASE_URL=x\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err = databaseURLFrom(locked)
	if want := "EACCES: permission denied, open '" + locked + "'"; err == nil || err.Error() != want {
		t.Fatalf("unreadable: %v, want %s", err, want)
	}
}
