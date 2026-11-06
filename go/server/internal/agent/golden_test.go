package agent

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// goldens is testdata/goldens.json, written by testdata/goldens.js from the
// Node server's own helpers (and V8's Date). Regenerate it with that script
// when the Node side changes.
type goldens struct {
	IsSafeFilename []struct {
		In  string `json:"in"`
		Out bool   `json:"out"`
	} `json:"is_safe_filename"`
	ExtractTitle []struct {
		Content  string `json:"content"`
		Filename string `json:"filename"`
		Out      string `json:"out"`
	} `json:"extract_title"`
	Dates []struct {
		In     string   `json:"in"`
		Truthy bool     `json:"truthy"`
		MS     *float64 `json:"ms"`
		Error  string   `json:"error"`
	} `json:"dates"`
}

func loadGoldens(t *testing.T) goldens {
	t.Helper()
	raw, err := os.ReadFile("testdata/goldens.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldens
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func parseValue(t *testing.T, text string) any {
	t.Helper()
	v, err := jsutil.Parse([]byte(text))
	if err != nil {
		t.Fatalf("%s: %v", text, err)
	}
	return v
}

func TestIsSafeFilenameMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).IsSafeFilename {
		if got := isSafeFilename(parseValue(t, c.In)); got != c.Out {
			t.Errorf("isSafeFilename(%s) = %v, Node %v", c.In, got, c.Out)
		}
	}
}

func TestExtractTitleMatchesNode(t *testing.T) {
	for _, c := range loadGoldens(t).ExtractTitle {
		if got := extractTitle(c.Content, c.Filename); got != c.Out {
			t.Errorf("extractTitle(%q, %q) = %q, Node %q", c.Content, c.Filename, got, c.Out)
		}
	}
}

// freeFormDates are values V8 reads with its legacy date parser, which the
// port does not have: Go refuses them (go-server note, Known differences).
var freeFormDates = map[string]bool{
	`"2026-11-06T15:07:00-0500"`: true, `"2026-11-06 15:07:00"`: true, `"2026-11-06 15:07:00Z"`: true,
	`"2026-11-06t15:07:00z"`: true, `"2026-11-06T15:07:00z"`: true, `"2026-11-06t15:07:00Z"`: true,
	`"10/09/2026"`: true, `"Oct 9 2026"`: true, `"Fri, 06 Nov 2026 15:07:00 GMT"`: true, `"2026/11/06"`: true,
	`"2026-11-6"`: true, `"12345-01-01"`: true, `"+12345-01-01"`: true, `"0"`: true, `[1, 2]`: true,
}

// The mtime check and comparison value, new Date(v).getTime(), over V8's
// results in America/New_York (local times and both DST edges included).
func TestDateValueMatchesNode(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	seen := map[string]bool{}
	for _, c := range loadGoldens(t).Dates {
		v := parseValue(t, c.In)
		if got := jsutil.Truthy(v); got != c.Truthy {
			t.Errorf("Truthy(%s) = %v, Node %v", c.In, got, c.Truthy)
		}
		ms, ok, err := jsutil.DateValue(v, loc)
		switch {
		case c.Error != "":
			if err == nil || err.Error() != c.Error {
				t.Errorf("DateValue(%s): error %v, Node %q", c.In, err, c.Error)
			}
		case err != nil:
			t.Errorf("DateValue(%s): %v", c.In, err)
		case freeFormDates[c.In]:
			seen[c.In] = true
			if c.MS == nil {
				t.Errorf("%s is listed as free-form but Node refuses it", c.In)
			}
			if ok {
				t.Errorf("DateValue(%s) = %v; free-form dates are refused", c.In, ms)
			}
		case c.MS == nil:
			if ok {
				t.Errorf("DateValue(%s) = %v, Node NaN", c.In, ms)
			}
		case !ok || ms != *c.MS:
			t.Errorf("DateValue(%s) = %v %v, Node %v", c.In, ms, ok, *c.MS)
		}
	}
	for in := range freeFormDates {
		if !seen[in] {
			t.Errorf("free-form %s is not in the goldens", in)
		}
	}
}
