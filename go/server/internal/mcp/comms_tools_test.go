package mcp

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// The expected values come from Node 24 running agent_status's own
// expressions (`a.expertise || []`, `.length > 0`, `.join(', ')`) over each
// jsonb value.
func TestExpertiseTextMatchesNode(t *testing.T) {
	cases := []struct {
		raw  string
		ok   bool
		text string
		err  string
	}{
		{raw: "null"},
		{raw: "[]"},
		{raw: `["go","postgres"]`, ok: true, text: "go, postgres"},
		{raw: `[null,1,true,[1,2],{"a":1},"x"]`, ok: true, text: ", 1, true, 1,2, [object Object], x"},
		{raw: `"abc"`, err: "expertise.join is not a function"},
		{raw: `""`},
		{raw: "0"},
		{raw: "5"},
		{raw: "true"},
		{raw: "false"},
		{raw: "{}"},
		{raw: `{"length":2}`, err: "expertise.join is not a function"},
		{raw: `{"length":"0"}`},
		{raw: `{"length":[3]}`, err: "expertise.join is not a function"},
		{raw: `[""]`, ok: true, text: ""},
		{raw: `["a",null]`, ok: true, text: "a, "},
	}
	for _, c := range cases {
		text, ok, err := expertiseText([]byte(c.raw))
		if c.err != "" {
			if err == nil || err.Error() != c.err {
				t.Errorf("%s: err %v, want %q", c.raw, err, c.err)
			}
			continue
		}
		if err != nil || ok != c.ok || text != c.text {
			t.Errorf("%s: (%q, %v, %v), want (%q, %v)", c.raw, text, ok, err, c.text, c.ok)
		}
	}
	if _, ok, err := expertiseText(nil); ok || err != nil {
		t.Error("SQL NULL must read as no expertise")
	}
}

func argsOf(t *testing.T, js string) *jsutil.Object {
	t.Helper()
	v, err := jsutil.Parse([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return v.(*jsutil.Object)
}

func TestDiscussionArg(t *testing.T) {
	cases := map[string]int64{
		`{}`:                       0,
		`{"discussion_id":0}`:      0,
		`{"discussion_id":-0}`:     0,
		`{"discussion_id":110}`:    110,
		`{"discussion_id":-3}`:     -3,
		`{"discussion_id":3e9}`:    3000000000,
		`{"discussion_id":1e18}`:   1000000000000000000,
		`{"discussion_id":9.2e18}`: 9200000000000000000,
		// -2^63 is exactly a double and an int64.
		`{"discussion_id":-9223372036854775808}`: math.MinInt64,
	}
	for js, want := range cases {
		got, err := discussionArg(argsOf(t, js))
		if err != nil || got != want {
			t.Errorf("%s: %d, %v; want %d", js, got, err, want)
		}
	}
	for _, js := range []string{`{"discussion_id":1.5}`, `{"discussion_id":-0.25}`, `{"discussion_id":1e19}`, `{"discussion_id":-9.3e18}`,
		// 2^63, and the double just below -2^63.
		`{"discussion_id":9223372036854775808}`, `{"discussion_id":-9223372036854777856}`} {
		_, err := discussionArg(argsOf(t, js))
		status, _ := apierr.StatusAndCode(err)
		if err == nil || status != 400 || err.Error() != "discussion_id must be an integer" {
			t.Errorf("%s: %v (status %d), want the 400", js, err, status)
		}
	}
}

func TestIdentity(t *testing.T) {
	c := caller{agent: "agent-a"}
	for _, js := range []string{`{}`, `{"agent":""}`, `{"agent":"agent-a"}`} {
		got, err := identity(argsOf(t, js), "agent", c)
		if err != nil || got != "agent-a" {
			t.Errorf("%s: %q, %v", js, got, err)
		}
	}
	_, err := identity(argsOf(t, `{"from":"Agent-A"}`), "from", c)
	var api *apierr.Error
	if !errors.As(err, &api) || api.Status != 403 || err.Error() != `Identity mismatch: from "Agent-A" does not match authenticated agent "agent-a"` {
		t.Errorf("mismatch: %v", err)
	}
}

func TestPreview(t *testing.T) {
	if preview("") != "" {
		t.Error("no preview must add nothing")
	}
	if got := preview("hello"); got != "\n> hello" {
		t.Errorf("short preview: %q", got)
	}
	if got := preview(strings.Repeat("a", 200)); !strings.HasSuffix(got, "a...") {
		t.Errorf("200 characters must end in ...: %q", got)
	}
	// 100 astral characters are 200 UTF-16 units: JavaScript's length.
	if got := preview(strings.Repeat("\U0001F600", 100)); !strings.HasSuffix(got, "...") {
		t.Errorf("200 UTF-16 units must end in ...: %q", got)
	}
	if got := preview(strings.Repeat("a", 199)); strings.HasSuffix(got, "...") {
		t.Errorf("199 characters must not end in ...: %q", got)
	}
}
