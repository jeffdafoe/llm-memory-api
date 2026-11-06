package pgjson

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// TestParseMatchesNode compares Parse with pg-types' text parsers
// (testdata/goldens.js, run with TZ=America/New_York).
func TestParseMatchesNode(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no zone data:", err)
	}
	saved := time.Local
	time.Local = loc
	defer func() { time.Local = saved }()

	data, err := os.ReadFile("testdata/goldens.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		OID  uint32 `json:"oid"`
		Text string `json:"text"`
		JSON string `json:"json"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		got, err := Parse(c.OID, c.Text)
		if err != nil {
			t.Errorf("%d %q: %v", c.OID, c.Text, err)
			continue
		}
		if string(got) != c.JSON {
			t.Errorf("%d %q: %s, Node %s", c.OID, c.Text, got, c.JSON)
		}
	}
}

func TestRowKeepsFirstPlaceOfARepeatedName(t *testing.T) {
	var r Row
	r.set("a", json.RawMessage("1"))
	r.set("b", json.RawMessage("2"))
	r.set("a", json.RawMessage("3"))
	got, err := json.Marshal(r)
	if err != nil || string(got) != `{"a":3,"b":2}` {
		t.Errorf("%s %v", got, err)
	}
	if s, ok := (Row{{Name: "x", Value: json.RawMessage(`"<&>"`)}}).String("x"); !ok || s != "<&>" {
		t.Errorf("String: %q %v", s, ok)
	}
}
