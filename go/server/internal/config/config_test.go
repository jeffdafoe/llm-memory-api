package config

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

func TestLoadAndGet(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery("SELECT key, value FROM config").
		WillReturnRows(pgxmock.NewRows([]string{"key", "value"}).
			AddRow("mcp_oauth_bearer_secret", "s3cret").
			AddRow("blank", ""))

	store := NewStore()
	if err := store.Load(context.Background(), mock); err != nil {
		t.Fatalf("Load: %v", err)
	}

	value, err := store.Get("mcp_oauth_bearer_secret")
	if err != nil || value != "s3cret" {
		t.Fatalf("Get = %q, %v", value, err)
	}
	value, err = store.Get("blank")
	if err != nil || value != "" {
		t.Fatalf("Get blank = %q, %v", value, err)
	}

	_, err = store.Get("missing")
	want := `Config key "missing" not found. Was config.init() called?`
	if err == nil || err.Error() != want {
		t.Fatalf("Get missing error = %v, want %q", err, want)
	}

	store.Set("missing", "now here")
	value, ok := store.Lookup("missing")
	if !ok || value != "now here" {
		t.Fatalf("Lookup after Set = %q, %v", value, ok)
	}
}

func TestParseNonNegativeFinite(t *testing.T) {
	cases := []struct {
		name     string
		value    string
		present  bool
		fallback float64
		want     float64
	}{
		{"absent", "", false, 7, 7},
		{"blank", "", true, 7, 7},
		{"spaces", "   ", true, 7, 7},
		{"zero is a real value", "0", true, 7, 0},
		{"decimal", "2.5", true, 7, 2.5},
		{"padded", " 12 ", true, 7, 12},
		{"exponent", "1e3", true, 7, 1000},
		{"leading dot", ".5", true, 7, 0.5},
		{"hex", "0x10", true, 7, 16},
		{"binary", "0b101", true, 7, 5},
		{"octal", "0o17", true, 7, 15},
		{"hex above uint64", "0x10000000000000000", true, 7, 18446744073709551616},
		{"upper-case prefix", "0XFF", true, 7, 255},
		{"negative", "-1", true, 7, 7},
		{"garbage", "12abc", true, 7, 7},
		{"infinity", "Infinity", true, 7, 7},
		{"underscore", "1_000", true, 7, 7},
		{"hex underscore", "0x1_0", true, 7, 7},
		{"binary underscore", "0b1_0", true, 7, 7},
		{"octal underscore", "0o1_0", true, 7, 7},
		{"hex float", "0x1p3", true, 7, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseNonNegativeFinite(tc.value, tc.present, tc.fallback)
			if got != tc.want {
				t.Fatalf("ParseNonNegativeFinite(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
