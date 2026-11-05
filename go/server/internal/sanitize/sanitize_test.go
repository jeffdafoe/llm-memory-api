package sanitize

import "testing"

func TestIdentifier(t *testing.T) {
	cases := map[string]string{
		"  notes/<script>x.md  ": "notes/scriptx.md",
		"<<>>":                   "",
		"Anne_Walker/ <b>":       "Anne_Walker/ b",
		"a\u00a0b\u00e9":         "a\u00a0b",
	}
	for in, want := range cases {
		if got := Identifier(in); got != want {
			t.Errorf("Identifier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContent(t *testing.T) {
	in := "Line\x00 one\u200b and\u202e two\t\r\n\u00e9\U0001F600\x7f\u0085\ufeff\u2066"
	if got, want := Content(in), "Line one and two\t\r\n\u00e9\U0001F600"; got != want {
		t.Errorf("Content = %q, want %q", got, want)
	}
}

func TestAgentName(t *testing.T) {
	if got := AgentName("  Agent-A_1 <x> \u212a"); got != "agent-a_1xk" {
		t.Errorf("AgentName = %q", got)
	}
}
