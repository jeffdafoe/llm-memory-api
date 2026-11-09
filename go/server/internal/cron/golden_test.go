package cron

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	// The zone database is embedded in the test binary only, so the DST
	// cases run on a machine without one.
	_ "time/tzdata"
)

// goldens is testdata/goldens.json, recorded from
// node-cron itself. Regenerate it with that script when node-cron changes.
type goldens struct {
	NodeCron string `json:"node_cron"`
	Validate []struct {
		In    string `json:"in"`
		Valid bool   `json:"valid"`
		Error string `json:"error"`
	} `json:"validate"`
	Next []struct {
		Zone    string   `json:"zone"`
		Pattern string   `json:"pattern"`
		Base    string   `json:"base"`
		Next    []string `json:"next"`
		Error   string   `json:"error"`
	} `json:"next"`
	Match []struct {
		Zone    string `json:"zone"`
		Pattern string `json:"pattern"`
		At      string `json:"at"`
		Match   bool   `json:"match"`
	} `json:"match"`
}

func loadGoldens(t *testing.T) goldens {
	t.Helper()
	data, err := os.ReadFile("testdata/goldens.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldens
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestValidateMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Validate {
		if got := Validate(c.In); got != c.Valid {
			t.Errorf("Validate(%q) = %v, node-cron gives %v", c.In, got, c.Valid)
		}
		_, err := Parse(c.In)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		// node-cron runs out of memory building such a range and throws
		// RangeError; the port gives the field's own message.
		if strings.HasPrefix(c.Error, "threw: ") {
			if err == nil {
				t.Errorf("Parse(%q) succeeded; node-cron threw", c.In)
			}
			continue
		}
		if msg != c.Error {
			t.Errorf("Parse(%q) error %q, node-cron gives %q", c.In, msg, c.Error)
		}
	}
}

func TestNextMatchesNode(t *testing.T) {
	g := loadGoldens(t)
	if len(g.Next) == 0 {
		t.Fatal("no next cases")
	}
	for _, c := range g.Next {
		sched, err := ParseIn(c.Pattern, zone(t, c.Zone))
		if err != nil {
			t.Fatalf("ParseIn(%q): %v", c.Pattern, err)
		}
		from, err := time.Parse(time.RFC3339Nano, c.Base)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		gotErr := ""
		for range 3 {
			next, err := sched.Next(from)
			if err != nil {
				gotErr = err.Error()
				break
			}
			got = append(got, next.UTC().Format("2006-01-02T15:04:05.000Z"))
			from = next
		}
		if strings.Join(got, " ") != strings.Join(c.Next, " ") || gotErr != c.Error {
			t.Errorf("%s %q from %s: %v %q, node-cron gives %v %q", c.Zone, c.Pattern, c.Base, got, gotErr, c.Next, c.Error)
		}
	}
}

func TestMatchMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).Match {
		sched, err := ParseIn(c.Pattern, zone(t, c.Zone))
		if err != nil {
			t.Fatal(err)
		}
		at, err := time.Parse(time.RFC3339Nano, c.At)
		if err != nil {
			t.Fatal(err)
		}
		if got := sched.Match(at); got != c.Match {
			t.Errorf("%s %q at %s: %v, node-cron gives %v", c.Zone, c.Pattern, c.At, got, c.Match)
		}
	}
}
