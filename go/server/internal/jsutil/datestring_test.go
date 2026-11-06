package jsutil

import (
	"testing"
	"time"
)

// The expected strings are String(new Date(...)) from Node 24.
func TestDateStringMatchesNode(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database:", err)
	}
	cases := []struct {
		t    time.Time
		loc  *time.Location
		want string
	}{
		{time.Date(2026, 11, 6, 14, 31, 30, 999999999, time.UTC), time.UTC, "Fri Nov 06 2026 14:31:30 GMT+0000 (Coordinated Universal Time)"},
		{time.Date(5, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC, "Sat Jan 01 0005 00:00:00 GMT+0000 (Coordinated Universal Time)"},
		{time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC), time.UTC, "Fri Jan 01 -0001 00:00:00 GMT+0000 (Coordinated Universal Time)"},
		{time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), newYork, "Wed Jul 01 2026 08:00:00 GMT-0400 (Eastern Daylight Time)"},
		{time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), newYork, "Thu Jan 01 2026 07:00:00 GMT-0500 (Eastern Standard Time)"},
	}
	for _, c := range cases {
		if got := DateString(c.t, c.loc); got != c.want {
			t.Errorf("%v: got %q, want %q", c.t, got, c.want)
		}
	}
}
