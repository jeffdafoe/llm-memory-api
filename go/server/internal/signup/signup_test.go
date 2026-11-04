package signup

import "testing"

// Expected values are Node's !Number.isNaN(Date.parse(s)) for each string,
// except the free-form dates at the end, which V8 accepts and this package
// deliberately does not.
func TestValidISODateMatchesNode(t *testing.T) {
	cases := map[string]bool{
		"2026-10-09T12:00:00.123456Z":    true,
		"2026-10-09T12:00:00.123456789Z": true,
		"2026-10-09T12:00:00Z":           true,
		"2026-10-09T12:00:00+0000":       true,
		"2026-10-09T12:00:00+00:00":      true,
		"2026-10-09T12:00:00.5-05:00":    true,
		"2026-10-09T12:00:00":            true,
		"2026-10-09T12:00":               true,
		"2026-10-09":                     true,
		"2026-10-09 12:00:00Z":           true,
		"2026-10-09 12:00:00":            true,
		"2026-10-09 12:00:00+00":         true,
		"soon":                           false,
		"":                               false,
		"2026-13-01":                     false,
		"2026-10-09T25:00:00Z":           false,
		"2026-10-09T12:00:00+15:59":      true,
		"2026-10-09T12:00:00+24:00":      false,
		"2026-10-09T12:00:00+12:60":      false,
		"2026-10-09T12:00:00+99:99":      false,
		"0001-01-01":                     true,
		// V8 says true; Postgres refuses these, so they are refused here.
		"2026-10-09T12:00:00+16:00": false,
		"2026-10-09T12:00:00+23:59": false,
		"0000-01-01":                false,
		// Free-form: V8 says true, refused here on purpose.
		"10/09/2026": false,
		"Oct 9 2026": false,
	}
	for value, want := range cases {
		if got := validISODate(value); got != want {
			t.Errorf("validISODate(%q) = %v, want %v", value, got, want)
		}
	}
}
