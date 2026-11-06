package main

import "testing"

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
