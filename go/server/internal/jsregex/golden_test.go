package jsregex

import (
	"encoding/json"
	"os"
	"testing"
)

// goldens is testdata/goldens.json, recorded from
// Node itself: for each pattern, V8's syntax verdict (with its message),
// safe-regex's verdict, and new RegExp(p, 'i').test(line) for every sample
// line.
type goldens struct {
	Lines    []string `json:"lines"`
	Patterns []struct {
		Pattern string `json:"pattern"`
		Error   string `json:"error"`
		Message string `json:"message"`
		Matches []bool `json:"matches"`
	} `json:"patterns"`
}

func TestMatchesNode(t *testing.T) {
	data, err := os.ReadFile("testdata/goldens.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldens
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	var invalid, unsafe, matched, compared int
	defer func() {
		t.Logf("%d patterns: %d invalid, %d unsafe, %d compiled; %d line results compared", len(g.Patterns), invalid, unsafe, matched, compared)
	}()
	for _, c := range g.Patterns {
		p, err := Parse(c.Pattern)
		switch c.Error {
		case "invalid":
			if err == nil || err.Error() != c.Message {
				t.Errorf("%q: got %v, V8 says %q", c.Pattern, err, c.Message)
			}
			invalid++
			continue
		case "length":
			continue
		}
		if err != nil {
			t.Errorf("%q: %v, V8 accepts it", c.Pattern, err)
			continue
		}
		if safe := p.Safe(); safe != (c.Error != "unsafe") {
			t.Errorf("%q: Safe() = %v, safe-regex says %v", c.Pattern, safe, c.Error != "unsafe")
		}
		if c.Error == "unsafe" {
			unsafe++
			continue
		}
		matched++
		re, err := CompileIgnoreCase(c.Pattern)
		if err != nil {
			t.Errorf("%q: %v", c.Pattern, err)
			continue
		}
		for i, line := range g.Lines {
			compared++
			got, err := re.MatchString(line, NewBudget(nil, 1_000_000))
			if err != nil {
				t.Errorf("%q on %q: %v", c.Pattern, line, err)
				continue
			}
			if got != c.Matches[i] {
				t.Errorf("%q on %q: %v, JavaScript gives %v", c.Pattern, line, got, c.Matches[i])
			}
		}
	}
}
