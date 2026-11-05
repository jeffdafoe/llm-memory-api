package pgbind

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsutil"
)

// TestMatchesNodePostgres compares Value with node-postgres's prepareValue
// over testdata/goldens.json (written by testdata/goldens.js).
func TestMatchesNodePostgres(t *testing.T) {
	data, err := os.ReadFile("testdata/goldens.json")
	if err != nil {
		t.Fatal(err)
	}
	var goldens []struct {
		Input    string  `json:"input"`
		Prepared *string `json:"prepared"`
	}
	if err := json.Unmarshal(data, &goldens); err != nil {
		t.Fatal(err)
	}
	for _, g := range goldens {
		got := Value(json.RawMessage(g.Input))
		var text *string
		switch v := got.(type) {
		case string:
			text = &v
		case jsutil.Number:
			s := string(v)
			text = &s
		case nil:
		default:
			t.Errorf("%s: unexpected type %T", g.Input, got)
			continue
		}
		switch {
		case g.Prepared == nil && text != nil:
			t.Errorf("%s: got %q, node-postgres binds NULL", g.Input, *text)
		case g.Prepared != nil && text == nil:
			t.Errorf("%s: got NULL, node-postgres binds %q", g.Input, *g.Prepared)
		case g.Prepared != nil && *text != *g.Prepared:
			t.Errorf("%s: got %q, node-postgres binds %q", g.Input, *text, *g.Prepared)
		}
	}
}

func TestTruthyAndOrDefault(t *testing.T) {
	falsy := []string{"", "null", "false", `""`, "0", "-0", " 0 ", "0.0"}
	for _, in := range falsy {
		if Truthy(json.RawMessage(in)) {
			t.Errorf("Truthy(%q) = true", in)
		}
		if got := OrDefault(json.RawMessage(in), 50); got != 50 {
			t.Errorf("OrDefault(%q) = %v, want 50", in, got)
		}
	}
	truthy := []string{"1", `"0"`, `" "`, "[]", "{}", "true", "-1"}
	for _, in := range truthy {
		if !Truthy(json.RawMessage(in)) {
			t.Errorf("Truthy(%q) = false", in)
		}
	}
	if got := OrDefault(json.RawMessage(`"20"`), 50); got != "20" {
		t.Errorf(`OrDefault("20") = %v`, got)
	}
	if got := OrDefault(json.RawMessage(`[5]`), 50); got != `{"5"}` {
		t.Errorf("OrDefault([5]) = %v, want the array literal node-postgres sends", got)
	}
}
