package jsutil

import "testing"

func TestReplaceAll(t *testing.T) {
	cases := []struct{ s, repl, want string }{
		{"Hi {agent}, {agent}!", "ann", "Hi ann, ann!"},
		{"a{agent}b", "$$", "a$b"},
		{"a{agent}b", "<$&>", "a<{agent}>b"},
		{"a{agent}b", "[$`]", "a[a]b"},
		{"a{agent}b", "[$']", "a[b]b"},
		{"a{agent}b", "$1$<x>$", "a$1$<x>$b"},
		{"{agent}{agent}", "$`", "{agent}"},
		{"none", "$&", "none"},
	}
	for _, c := range cases {
		if got := ReplaceAll(c.s, "{agent}", c.repl); got != c.want {
			t.Errorf("%q with %q = %q, want %q", c.s, c.repl, got, c.want)
		}
	}
}
