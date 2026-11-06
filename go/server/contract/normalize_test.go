package contract

import (
	"strings"
	"testing"
)

func TestNormalizeTimestamps(t *testing.T) {
	n := NewNormalizer()
	cases := map[string]string{
		"2026-11-05T14:22:55.311Z":              "<ts>",
		"2026-11-05T14:22:55.311234+00:00":      "<ts>",
		"2026-11-05 14:22:55.311234+00":         "<ts>",
		"2026-11-05T14:22:55":                   "<ts>",
		"expires 2026-11-05T14:22:55.311Z soon": "expires <ts> soon",
		"2026-11-05":                            "2026-11-05",
	}
	for in, want := range cases {
		if got := n.String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeRandomValuesFirstSeenOrder(t *testing.T) {
	hex64 := strings.Repeat("ab12", 16)
	otherHex := strings.Repeat("cd34", 16)
	uuid := "3f2504e0-4f89-11d3-9a0c-0305e82c3301"
	b64 := "Zk9vYmFyQmF6UXV4MTIzNDU2Nzg5MGFiY2RlZg_-"

	n := NewNormalizer()
	got := []string{
		n.String(hex64),
		n.String(uuid),
		n.String("Bearer " + otherHex),
		n.String(b64),
		n.String("again " + hex64),
		n.String("key_" + otherHex),
	}
	want := []string{
		"<rand-1>",
		"<rand-2>",
		"Bearer <rand-3>",
		"<rand-4>",
		"again <rand-1>",
		"key_<rand-3>",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("case %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// A long random token can lack one character class by chance; from 40
// characters two classes are enough.
func TestNormalizeLongTwoClassToken(t *testing.T) {
	n := NewNormalizer()
	if got := n.String(strings.Repeat("g7q_", 16)); got != "<rand-1>" {
		t.Errorf("64-char lower+digit token: got %q", got)
	}
	short := strings.Repeat("g7q", 13) // 39 characters
	if got := n.String(short); got != short {
		t.Errorf("39-char two-class run renamed: %q", got)
	}
}

func TestNormalizeLeavesIdentifiersAlone(t *testing.T) {
	n := NewNormalizer()
	for _, s := range []string{
		"virtual_agent_error_ping_interval_seconds",
		"mcp_oauth_bearer_secret",
		"abcdef0123456789abcdef012345678", // 31 hex: too short
		"INVALID_TOKEN_OR_API_KEY_FOR_THIS_SERVER",
	} {
		if got := n.String(s); got != s {
			t.Errorf("String(%q) = %q, want it unchanged", s, got)
		}
	}
}

func TestNormalizeOrigins(t *testing.T) {
	n := NewNormalizer("http://127.0.0.1:41234")
	got := n.String(`Bearer resource_metadata="http://127.0.0.1:41234/.well-known/x" host 127.0.0.1:41234`)
	want := `Bearer resource_metadata="<origin>/.well-known/x" host <origin>`
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeValueKeysAndStrings(t *testing.T) {
	hex := strings.Repeat("0f", 32)
	v, _ := ParseJSON([]byte(`{"` + hex + `":{"at":"2026-01-02T03:04:05.000Z","n":12}}`))
	got := NewNormalizer().Value(v).Compact()
	want := `{"<rand-1>":{"at":"<ts>","n":12}}`
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

// The same rows in a different order must normalize to the same result,
// random-value names included.
func TestSortMultisetMakesNamesOrderFree(t *testing.T) {
	tokA := strings.Repeat("a1", 32)
	tokB := strings.Repeat("b2", 32)
	row := func(path, tok string) Value {
		v, _ := ParseJSON([]byte(`{"path":"` + path + `","token":"` + tok + `"}`))
		return v
	}
	first := []Value{row("/x", tokA), row("/y", tokB)}
	second := []Value{row("/y", tokB), row("/x", tokA)}

	norm := func(rows []Value) string {
		n := NewNormalizer()
		var parts []string
		for _, r := range n.SortMultiset(rows) {
			parts = append(parts, n.Value(r).Compact())
		}
		return strings.Join(parts, " ")
	}
	if a, b := norm(first), norm(second); a != b {
		t.Fatalf("order changed the result:\n%s\n%s", a, b)
	}
}

// Rows that differ only in which already-named token they carry must sort by
// that name, not tie: otherwise Node's [A, B] and Go's [B, A] would number
// the same multiset differently.
func TestSortMultisetUsesKnownNames(t *testing.T) {
	tokA := strings.Repeat("a1", 32)
	tokB := strings.Repeat("b2", 32)
	row := func(tok string) Value {
		v, _ := ParseJSON([]byte(`{"path":"/x","token":"` + tok + `"}`))
		return v
	}
	norm := func(rows []Value) string {
		n := NewNormalizer()
		n.String(tokA) // named <rand-1> by an earlier response
		n.String(tokB) // <rand-2>
		var parts []string
		for _, r := range n.SortMultiset(rows) {
			parts = append(parts, n.Value(r).Compact())
		}
		return strings.Join(parts, " ")
	}
	a := norm([]Value{row(tokA), row(tokB)})
	b := norm([]Value{row(tokB), row(tokA)})
	if a != b {
		t.Fatalf("order changed the result:\n%s\n%s", a, b)
	}
}

func TestAddRandomRenamesWholeValuesOnly(t *testing.T) {
	n := NewNormalizer()
	n.AddRandom("plaza-oyster-tidal")
	n.AddRandom("1762378800")
	if v := n.Value(Str("plaza-oyster-tidal")); v.Str != "<rand-1>" {
		t.Fatalf("%+v", v)
	}
	if got := n.String("pass plaza-oyster-tidal"); got != "pass plaza-oyster-tidal" {
		t.Fatalf("substring renamed: %q", got)
	}
	v := n.Value(Value{Kind: Number, Num: "1762378800"})
	if v.Kind != Number || v.Num != "<rand-2>" {
		t.Fatalf("%+v", v)
	}
	if v := n.Value(Value{Kind: Number, Num: "5"}); v.Kind != Number || v.Num != "5" {
		t.Fatalf("%+v", v)
	}
}

// A short value declared random does not hide a difference in text that
// contains it, and a number against a string at a random path still differs.
func TestRandomValuesDoNotHideDifferences(t *testing.T) {
	side := func(msg string, issued Value) Result {
		body := Value{Kind: Object, Members: []Member{{Key: "issued", Value: issued}, {Key: "msg", Value: Str(msg)}}}
		return Result{Steps: []StepResult{{Label: "GET /x", Response: Response{Status: 200, Body: Body{IsJSON: true, JSON: body}}}}}
	}
	normalize := func(r Result, raw string) Result {
		n := NewNormalizer()
		n.AddRandom(raw)
		r.Steps[0].Response.Body.JSON = n.Value(r.Steps[0].Response.Body.JSON)
		return r
	}
	node := normalize(side("took 1 try", Value{Kind: Number, Num: "1"}), "1")
	goRes := normalize(side("took 2 tries", Value{Kind: Number, Num: "1"}), "1")
	if rep, _ := Compare(node, goRes, nil); rep.OK() {
		t.Fatal("a difference in text containing the random value was hidden")
	}
	node = normalize(side("same", Value{Kind: Number, Num: "123"}), "123")
	goRes = normalize(side("same", Str("123")), "123")
	if rep, _ := Compare(node, goRes, nil); rep.OK() {
		t.Fatal("a number against a string at a random path compared equal")
	}
}
